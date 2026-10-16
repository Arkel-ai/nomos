import { fetchApi } from './fetchApi';
import { queryString, unwrapRun as unwrap } from './runs';

/**
 * Retrieval (§6, §10). Two corpora that behave differently on purpose:
 * `public` is RRF(HNSW, french FTS) then rerank; `private` is an exact scan
 * after the tenant + dossier filter, with no HNSW.
 *
 * Two things this client must never let the UI claim (§6.3): RRF is a
 * candidate generator and a reranker optimises topical relevance. Neither
 * establishes whether a decision was overruled, distinguished or superseded.
 * Surface the candidates with their jurisdiction, formation, publication level
 * and dates — never assert that the law changed.
 */

export type Corpus = 'public' | 'private' | 'both';

/** Switchable so the relevance harness scores the same code path we ship. */
export type SearchStrategy = 'vector' | 'fts' | 'rrf' | 'rrf_rerank';

export interface SearchRequest {
	query: string;
	corpus?: Corpus;
	/** Required whenever the corpus includes private: a private read without a
	 *  dossier scope is a bug, not a broad search. */
	dossier_id?: string | null;
	/** The matter date temporal filtering runs against. 0 means "today", which
	 *  is only correct for a current dispute (§6.4). */
	as_of_date?: number;
	limit?: number;
}

export interface SearchHit {
	corpus: Corpus;
	chunk_id: number;
	text: string;
	score: number;
	rerank_rank: number;

	// Public provenance.
	legal_version_id?: string;
	citation_label?: string;
	canonical_url?: string;
	license_code?: string;
	valid_from?: number | null;
	valid_to?: number | null;

	// Private provenance.
	piece_id?: string;
	page_from?: number;
	page_to?: number;

	// What the candidate actually is — the metadata precedential weight needs.
	source?: string;
	kind?: string;
	jurisdiction?: string;
	formation?: string;
	publication_level?: string;
	solution?: string;
	decision_date?: number | null;
	ecli?: string;
	pourvoi?: string;
	version_state?: string;

	// How it was ranked.
	rrf_score?: number;
	vector_rank?: number;
	fts_rank?: number;
	vector_score?: number;
	fts_score?: number;
}

/**
 * One visible source credit. Licence Ouverte 2.0 permits commercial reuse
 * WITH attribution, so this is a product requirement, not a footer.
 */
export interface Attribution {
	source: string;
	license_code?: string;
	notice: string;
	hits: number;
}

export interface SearchResponse {
	results: SearchHit[];
	/** False means the rerank pass could not run and the raw fusion order is
	 *  what you are looking at. The UI must not claim otherwise. */
	reranked: boolean;
	strategy: SearchStrategy | string;
	corpus: Corpus | string;
	as_of_date: number;
	candidates: number;
	attribution: Attribution[];
}

/** POST /search */
export async function search(
	body: SearchRequest,
	strategy?: SearchStrategy
): Promise<SearchResponse> {
	return unwrap(
		await fetchApi<SearchResponse>(`/search${queryString({ strategy })}`, 'POST', body)
	);
}
