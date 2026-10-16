import { fetchApi } from './fetchApi';
import {
	queryString,
	unwrapRun as unwrap,
	type Artifact,
	type ListQuery,
	type ListResponse
} from './runs';

/**
 * Dossiers — the document collections a conversation binds to (§10, §11).
 *
 * A dossier is the tenant-scoped container for the pièces of one matter. It is
 * secondary surface: threads are the product, dossiers are what a thread reads
 * from and writes back into.
 */

export type DossierState = 'active' | 'archived' | 'purge_pending';

/** The matter types the UI offers; the column itself is a free string. */
export const matterTypes = [
	'travail',
	'commercial',
	'civil',
	'administratif',
	'penal',
	'autre'
] as const;
export type MatterType = (typeof matterTypes)[number];

export interface Dossier {
	id: string;
	tenant_id: string;
	title: string;
	client_reference: string;
	matter_type: MatterType | string;
	state: DossierState;
	/** The title is a placeholder the thread's next turn replaces with a generated one. */
	title_pending: boolean;
	created_by: string;
	/** Unix seconds after which the explicit purge workflow may run. */
	purge_after: number;
	created_at: number;
	updated_at: number;
}

/** GET /dossiers/:id — the dossier plus its counters. */
export interface DossierDetail extends Dossier {
	piece_count: number;
	approved_count: number;
	run_count: number;
}

export interface DossierCreateRequest {
	title: string;
	client_reference?: string;
	matter_type?: MatterType | string;
	/** `title` is a placeholder: the next turn of the thread bound to it names the dossier. */
	auto_title?: boolean;
}

export interface ExportRequest {
	/** Pins the bordereau template; empty means the controlled default. */
	template_version?: string;
	/**
	 * Records that the lawyer signed off on the pièces as they stand. Without
	 * it the export is stamped "projet — vérification de l'avocat requise".
	 */
	approve?: boolean;
}

/** POST /dossiers */
export async function createDossier(body: DossierCreateRequest): Promise<Dossier> {
	return unwrap(await fetchApi<Dossier>('/dossiers', 'POST', body));
}

/** GET /dossiers — newest-updated first, paginated with a total. */
export async function listDossiers(query: ListQuery = {}): Promise<ListResponse<Dossier>> {
	return unwrap(await fetchApi<ListResponse<Dossier>>(`/dossiers${queryString({ ...query })}`));
}

/** GET /dossiers/:id */
export async function getDossier(id: string): Promise<DossierDetail> {
	return unwrap(await fetchApi<DossierDetail>(`/dossiers/${id}`));
}

/**
 * DELETE /dossiers/:id — starts the explicit purge workflow. There is no soft
 * delete: a soft delete purges nothing in S3, in the chunks or in the backups.
 */
export async function deleteDossier(id: string): Promise<DossierPurge> {
	return unwrap(await fetchApi<DossierPurge>(`/dossiers/${id}`, 'DELETE'));
}

/**
 * What a purge did. `complete: false` means some stored originals could not
 * be erased: the dossier stays `purge_pending` and the DELETE should be run
 * again.
 */
export interface DossierPurge {
	status: 'purged';
	id: string;
	complete: boolean;
	pieces: number;
	runs: number;
	objects_removed: number;
	objects_kept: number;
}

/** One renamed original as it lands in the archive. */
export interface ExportEntry {
	piece_id: string;
	ordinal: number;
	/** The name inside the zip — renaming is metadata, applied here only. */
	name: string;
	/** The name the file was uploaded under, never rewritten in the store. */
	original: string;
}

/**
 * What POST /dossiers/:id/export returns. The endpoint is SYNCHRONOUS — it
 * renders, validates and publishes before it answers — so the response carries
 * the published artifacts themselves. There is no run to follow.
 */
export interface ExportResult {
	artifact_id: string;
	bordereau_artifact_id: string;
	artifacts: Artifact[];
	name: string;
	/** False means both documents are stamped "projet". */
	approved: boolean;
	draft_notice?: string;
	entries: ExportEntry[];
	template_version: string;
	renderer_version: string;
	export_sha256: string;
	bordereau_sha256: string;
}

/**
 * POST /dossiers/:id/export — the ZIP of renamed originals plus the
 * bordereau.docx, rendered and published in the request. Without `approve`
 * both documents are stamped "projet — vérification de l'avocat requise".
 */
export async function exportDossier(
	id: string,
	body: ExportRequest = {}
): Promise<ExportResult> {
	return unwrap(await fetchApi<ExportResult>(`/dossiers/${id}/export`, 'POST', body));
}
