import { fetchApi } from './fetchApi';
import { unwrapRun as unwrap } from './runs';

/**
 * Public-corpus administration (§5, §9.1, §10).
 *
 * Loading the corpus is an ADMIN ACTION, never deployment behaviour: building
 * a vector index at boot against millions of rows turns a rollout into a
 * timeout. Both routes are admin-gated server-side; the rail hides them for
 * everyone else, which is UX only.
 */

export type CorpusSource = 'legifrance' | 'judilibre' | 'dila' | 'legi_hf';
export type CorpusSyncState = 'idle' | 'running' | 'failed';

export interface CorpusSync {
	id: string;
	source: CorpusSource | string;
	/** Where the last run stopped, so the next one resumes rather than restarts. */
	cursor: string;
	state: CorpusSyncState;
	stats?: Record<string, unknown>;
	last_error: string;
	last_run_at: number | null;
	created_at: number;
	updated_at: number;
}

export interface CorpusSyncRequest {
	source: CorpusSource;
	/** Forces a rebuild from the start rather than resuming the cursor. */
	full?: boolean;
}

/** GET /admin/corpus/status — cursors plus the counters that show §5.3 holds. */
export interface CorpusStatusResponse {
	syncs: CorpusSync[];
	documents: number;
	versions: number;
	chunks: number;
	embedded: number;
	/** False means vector search is degraded: the HNSW index is not built. */
	index_ready: boolean;
}

/** POST /admin/corpus/sync */
export async function syncCorpus(body: CorpusSyncRequest): Promise<CorpusSync> {
	return unwrap(await fetchApi<CorpusSync>('/admin/corpus/sync', 'POST', body));
}

/** GET /admin/corpus/status */
export async function getCorpusStatus(): Promise<CorpusStatusResponse> {
	return unwrap(await fetchApi<CorpusStatusResponse>('/admin/corpus/status'));
}
