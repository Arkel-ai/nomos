import { fetchApi } from './fetchApi';
import {
	queryString,
	unwrapRun as unwrap,
	type ListQuery,
	type ListResponse
} from './runs';

/**
 * Persistent style memory (§8, §10).
 *
 * Memory is explicit, inspectable, editable and deletable, and it distinguishes
 * what the user approved from what the model merely inferred. Nomos never
 * turns conversation text into permanent firm memory on its own: an inferred
 * entry is created `proposed` and is not used until it is approved. The UI must
 * keep that distinction visible — it is a consent problem before it is a
 * feature.
 */

export type MemoryScope = 'user' | 'firm';
export type MemoryState = 'proposed' | 'approved' | 'revoked';
export type MemoryProvenance = 'user_approved' | 'inferred';

export interface MemoryPreference {
	id: string;
	tenant_id: string;
	user_id: string;
	scope: MemoryScope | string;
	key: string;
	value?: Record<string, unknown>;
	provenance: MemoryProvenance | string;
	state: MemoryState;
	approved_at: number | null;
	expires_at: number | null;
	created_at: number;
	updated_at: number;
}

export interface MemoryUpsertRequest {
	scope: MemoryScope;
	key: string;
	value?: Record<string, unknown>;
	/** Defaults to user_approved server-side; an inferred entry lands `proposed`. */
	provenance?: MemoryProvenance;
	expires_at?: number | null;
}

/** GET /memory — the caller's preferences, paginated. */
export async function listMemory(
	query: ListQuery = {}
): Promise<ListResponse<MemoryPreference>> {
	return unwrap(
		await fetchApi<ListResponse<MemoryPreference>>(`/memory${queryString({ ...query })}`)
	);
}

/** POST /memory — create or replace one preference. */
export async function upsertMemory(body: MemoryUpsertRequest): Promise<MemoryPreference> {
	return unwrap(await fetchApi<MemoryPreference>('/memory', 'POST', body));
}

/**
 * Promotes a proposed entry. This is the only path from inferred to used, and
 * it is always a human action.
 */
export async function approveMemory(entry: MemoryPreference): Promise<MemoryPreference> {
	return upsertMemory({
		scope: entry.scope as MemoryScope,
		key: entry.key,
		value: entry.value,
		provenance: 'user_approved',
		expires_at: entry.expires_at
	});
}

/** DELETE /memory/:id */
export async function deleteMemory(id: string): Promise<void> {
	unwrap(await fetchApi(`/memory/${id}`, 'DELETE'));
}
