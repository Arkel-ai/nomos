import { derived, get, writable, type Readable } from 'svelte/store';
import {
	getDossier,
	listDossiers,
	type Dossier,
	type DossierDetail
} from '$lib/api/dossiers';
import { listPieces, updatePiece, type Piece, type PieceUpdateRequest } from '$lib/api/pieces';

/**
 * Dossiers — the collections a thread binds to (docs/PLAN.md §11).
 *
 * Two things live here: the tenant's dossier list (the picker behind the
 * collection pill) and the dossier currently open, with its pièces. The pièces
 * table is the one place where every proposed field is corrected before export,
 * so corrections apply optimistically and roll back on failure rather than
 * making the lawyer wait on a round trip per keystroke.
 */

interface DossierListState {
	loading: boolean;
	loaded: boolean;
	items: Dossier[];
	total: number;
	error: string | null;
}

interface CurrentDossierState {
	id: string | null;
	loading: boolean;
	dossier: DossierDetail | null;
	pieces: Piece[];
	pieceTotal: number;
	error: string | null;
}

function messageOf(err: unknown, fallback: string): string {
	return (err as { message?: string })?.message ?? fallback;
}

// ---------------------------------------------------------------------------
// The list
// ---------------------------------------------------------------------------

export const dossiers = writable<DossierListState>({
	loading: false,
	loaded: false,
	items: [],
	total: 0,
	error: null
});

let listInflight: Promise<Dossier[]> | null = null;

/** Loads the dossier list once; pass force after a create or a delete. */
export function loadDossiers(force = false): Promise<Dossier[]> {
	const current = get(dossiers);
	if (!force && current.loaded) return Promise.resolve(current.items);
	if (listInflight && !force) return listInflight;

	listInflight = (async () => {
		dossiers.update((s) => ({ ...s, loading: true, error: null }));
		try {
			const page = await listDossiers({ limit: 100 });
			dossiers.set({
				loading: false,
				loaded: true,
				items: page.items ?? [],
				total: page.total ?? 0,
				error: null
			});
			return page.items ?? [];
		} catch (err) {
			dossiers.set({
				loading: false,
				loaded: true,
				items: [],
				total: 0,
				error: messageOf(err, 'dossiers.loadError')
			});
			return [];
		} finally {
			listInflight = null;
		}
	})();
	return listInflight;
}

// ---------------------------------------------------------------------------
// The dossier currently open
// ---------------------------------------------------------------------------

export const currentDossier = writable<CurrentDossierState>({
	id: null,
	loading: false,
	dossier: null,
	pieces: [],
	pieceTotal: 0,
	error: null
});

/** Loads one dossier and its pièces, ordered as the bordereau will read. */
export async function loadDossier(id: string): Promise<void> {
	const current = get(currentDossier);
	currentDossier.set(
		current.id === id
			? { ...current, loading: true, error: null }
			: { id, loading: true, dossier: null, pieces: [], pieceTotal: 0, error: null }
	);

	try {
		const [detail, page] = await Promise.all([getDossier(id), listPieces(id, { limit: 200 })]);
		currentDossier.set({
			id,
			loading: false,
			dossier: detail,
			pieces: page.items ?? [],
			pieceTotal: page.total ?? 0,
			error: null
		});
		watchQualification(id);
	} catch (err) {
		currentDossier.update((s) => ({
			...s,
			loading: false,
			error: messageOf(err, 'dossiers.loadError')
		}));
	}
}

/** Re-reads the pièces alone — what a finished ingestion run should trigger. */
export async function refreshPieces(id: string): Promise<void> {
	try {
		const page = await listPieces(id, { limit: 200 });
		currentDossier.update((s) =>
			s.id === id ? { ...s, pieces: page.items ?? [], pieceTotal: page.total ?? 0 } : s
		);
	} catch (err) {
		currentDossier.update((s) =>
			s.id === id ? { ...s, error: messageOf(err, 'pieces.loadError') } : s
		);
	}
}

/**
 * Applies a lawyer correction optimistically and rolls it back if the API
 * refuses it. The server row always wins on success.
 */
export async function correctPiece(pieceId: string, patch: PieceUpdateRequest): Promise<void> {
	const before = get(currentDossier).pieces;
	currentDossier.update((s) => ({
		...s,
		error: null,
		pieces: s.pieces.map((p) => (p.id === pieceId ? { ...p, ...patch } : p))
	}));

	try {
		const saved = await updatePiece(pieceId, patch);
		currentDossier.update((s) => ({
			...s,
			pieces: s.pieces.map((p) => (p.id === pieceId ? saved : p))
		}));
	} catch (err) {
		currentDossier.update((s) => ({
			...s,
			pieces: before,
			error: messageOf(err, 'pieces.saveError')
		}));
		throw err;
	}
}

// ---------------------------------------------------------------------------
// The qualification watch
// ---------------------------------------------------------------------------

/**
 * Ingestion is asynchronous: extraction, chunking and qualification run in the
 * runner pools, so an upload lands as `pending` rows and the type, the date and
 * the titre appear over the following minutes, one pièce at a time. Read once,
 * the table shows whatever happened to be ready — which reads as "only the
 * first file was qualified". So the pièces are re-read while any of them is
 * still in flight, and the poll stops the moment they all settle.
 */
const PIECE_POLL_MS = 3000;
const PIECE_POLL_CEILING_MS = 15 * 60 * 1000;

let pollTimer: ReturnType<typeof setTimeout> | null = null;
let pollUntil = 0;

const isAnalysing = (p: Piece) =>
	p.classification_state === 'pending' || p.classification_state === 'extracted';

function schedulePoll(id: string): void {
	if (pollTimer) return;
	const state = get(currentDossier);
	if (state.id !== id || !state.pieces.some(isAnalysing) || Date.now() > pollUntil) return;
	pollTimer = setTimeout(async () => {
		pollTimer = null;
		await refreshPieces(id);
		schedulePoll(id);
	}, PIECE_POLL_MS);
}

/** Starts — or extends — the watch. Called on opening a dossier and after an
 *  upload; the ceiling stops a stalled run from polling for ever. */
export function watchQualification(id: string): void {
	pollUntil = Date.now() + PIECE_POLL_CEILING_MS;
	schedulePoll(id);
}

export function stopQualificationWatch(): void {
	if (pollTimer) clearTimeout(pollTimer);
	pollTimer = null;
}

/** Applies a title the API generated, wherever this dossier is on screen. */
export function renameDossier(id: string, title: string): void {
	currentDossier.update((s) =>
		s.id === id && s.dossier ? { ...s, dossier: { ...s.dossier, title, title_pending: false } } : s
	);
	dossiers.update((s) => ({
		...s,
		items: s.items.map((d) => (d.id === id ? { ...d, title, title_pending: false } : d))
	}));
}

export function resetCurrentDossier(): void {
	stopQualificationWatch();
	currentDossier.set({
		id: null,
		loading: false,
		dossier: null,
		pieces: [],
		pieceTotal: 0,
		error: null
	});
}

// ---------------------------------------------------------------------------
// Derived views
// ---------------------------------------------------------------------------

/** What the collection pill shows: `Collection du projet · 6`. */
export const collection: Readable<{ id: string; title: string; count: number } | null> = derived(
	currentDossier,
	($d) =>
		$d.dossier
			? {
					id: $d.dossier.id,
					title: $d.dossier.title,
					count: $d.dossier.piece_count ?? $d.pieceTotal
				}
			: null
);

/** The pièces the runner is still working on — stored but not yet qualified. */
export const piecesAnalysing: Readable<Piece[]> = derived(currentDossier, ($d) =>
	$d.pieces.filter(isAnalysing)
);

/** The pièces still waiting on the lawyer before an export can be approved. */
export const piecesAwaitingReview: Readable<Piece[]> = derived(currentDossier, ($d) =>
	$d.pieces.filter(
		(p) => p.classification_state !== 'approved' && p.classification_state !== 'rejected'
	)
);
