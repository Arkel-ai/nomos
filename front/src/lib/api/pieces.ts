import { fetchApi } from './fetchApi';
import {
	queryString,
	unwrapRun as unwrap,
	type DownloadLink,
	type ListQuery,
	type ListResponse
} from './runs';

/**
 * Pièces — the documents of one dossier (§7.1, §10).
 *
 * Two rules the UI must never soften: the stored original is never modified or
 * renamed (renaming is metadata, applied at export only), and every proposed
 * field is editable by the lawyer before export. Low confidence is surfaced,
 * never silently accepted.
 */

export type PieceState = 'pending' | 'extracted' | 'proposed' | 'approved' | 'rejected' | 'failed';

export type DatePrecision = 'day' | 'month' | 'year' | 'unknown';

export type PartyRole = 'emetteur' | 'destinataire' | 'partie' | 'tiers' | 'represente';

/** The CLOSED vocabulary the API qualifies against and validates corrections
 *  against — `pieceKinds` in api/services/qualify.go, same values, same order.
 *  It must stay in sync: a select offering a kind the API rejects is a 400, and
 *  one the rubrique catalogue does not know files the piece under "Autres
 *  pièces" without telling anyone. */
export const pieceKinds = [
	'contrat_de_travail',
	'avenant',
	'bulletin_de_paie',
	'lettre_de_licenciement',
	'mise_en_demeure',
	'convocation_entretien_prealable',
	'attestation',
	'courriel',
	'kbis',
	'facture',
	'jugement',
	'conclusions',
	'proces_verbal',
	'certificat_medical',
	'autre'
] as const;
export type PieceKind = (typeof pieceKinds)[number];

export interface Piece {
	id: string;
	tenant_id: string;
	dossier_id: string;
	source_asset_id: string;
	/** The name the file arrived with. Never rewritten. */
	original_name: string;
	/** What the qualification step proposes. */
	proposed_name: string;
	/** What the lawyer settled on; used at export. */
	final_name: string;
	ordinal: number;
	document_date: number | null;
	date_precision: DatePrecision | string;
	piece_kind: PieceKind | string;
	title: string;
	classification_state: PieceState;
	confidence: number;
	extraction_version: string;
	extraction?: Record<string, unknown>;
	page_count: number;
	text_chars: number;
	created_at: number;
	updated_at: number;
}

/** A party named on a pièce, kept as a row so timelines stay queryable. */
export interface PieceParty {
	id: string;
	tenant_id: string;
	piece_id: string;
	role: PartyRole | string;
	display_name: string;
	normalized_name: string;
	created_at: number;
}

/** Every proposed field is a lawyer correction; all of them are optional. */
export interface PieceUpdateRequest {
	proposed_name?: string;
	final_name?: string;
	ordinal?: number;
	document_date?: number | null;
	date_precision?: DatePrecision;
	piece_kind?: PieceKind | string;
	title?: string;
	classification_state?: PieceState;
}

/** One refused upload, reported rather than silently dropped. */
export interface RejectedUpload {
	name: string;
	reason: string;
}

export interface PieceUploadResponse {
	accepted: Piece[];
	rejected: RejectedUpload[];
	/** Ingestion and qualification run as a mission; follow it on the cursor. */
	run_id?: string;
}

/**
 * POST /dossiers/:id/pieces — multipart, n files. fetchApi forwards FormData
 * as-is, so the browser sets the multipart boundary itself.
 */
export async function uploadPieces(
	dossierId: string,
	files: File[] | FileList
): Promise<PieceUploadResponse> {
	const form = new FormData();
	for (const file of Array.from(files)) form.append('files', file);
	return unwrap(
		await fetchApi<PieceUploadResponse>(`/dossiers/${dossierId}/pieces`, 'POST', form)
	);
}

/** GET /dossiers/:id/pieces — ordered by ordinal, paginated with a total. */
export async function listPieces(
	dossierId: string,
	query: ListQuery = {}
): Promise<ListResponse<Piece>> {
	return unwrap(
		await fetchApi<ListResponse<Piece>>(`/dossiers/${dossierId}/pieces${queryString({ ...query })}`)
	);
}

/** PATCH /pieces/:id — the lawyer correction. */
export async function updatePiece(id: string, patch: PieceUpdateRequest): Promise<Piece> {
	return unwrap(await fetchApi<Piece>(`/pieces/${id}`, 'PATCH', patch));
}

/** Approving is a state transition like any other correction. */
export async function approvePiece(id: string): Promise<Piece> {
	return updatePiece(id, { classification_state: 'approved' });
}

/** Excluding a pièce keeps it stored; it just leaves the bordereau. */
export async function rejectPiece(id: string): Promise<Piece> {
	return updatePiece(id, { classification_state: 'rejected' });
}

/** GET /pieces/:id/download — a fresh presigned URL (the bucket is private). */
export async function getPieceDownloadLink(id: string): Promise<DownloadLink> {
	return unwrap(await fetchApi<DownloadLink>(`/pieces/${id}/download`));
}

/**
 * Download the stored original: ask for a signed URL on click, then let the
 * browser fetch it straight from S3. The signed URL is never stored or logged.
 */
export async function downloadPiece(id: string): Promise<void> {
	const link = await getPieceDownloadLink(id);
	const a = document.createElement('a');
	a.href = link.url;
	a.download = link.name;
	a.rel = 'noopener';
	document.body.appendChild(a);
	a.click();
	a.remove();
}

export type { DownloadLink };
