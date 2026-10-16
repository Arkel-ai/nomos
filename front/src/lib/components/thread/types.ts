import type { AgentCitation } from '$lib/api/agent-contracts';
import type { QuestionAnswer, StepState } from '$lib/api/runs';

/**
 * View shapes the thread owns.
 *
 * Everything the API already models — messages, runs, steps, questions,
 * artifacts — is imported from `$lib/api/*`. What lives here is what the thread
 * draws but no slice-1 endpoint returns yet, plus the loose contract the
 * composer hands back.
 */

/**
 * The three states a citation is allowed to be in, plus drift (docs/PLAN.md
 * §6.4, §13). They are NEVER collapsed into one badge: a source that resolves
 * is not a correct legal proposition, and only `lawyer_verified` means a human
 * took responsibility.
 */
export type CitationState = 'retrieved' | 'machine_checked' | 'lawyer_verified' | 'drifted';

/** Where a citation points: the open corpus, or a pièce of the dossier. */
export type CitationSourceType = 'legal' | 'piece';

/**
 * One frozen citation as the thread renders it — the citation row of §9
 * reduced to what the turn shows. `citations` is a slice-2 table, so the API
 * client has no shape for it yet; the thread accepts these from whoever can
 * supply them and renders nothing when the list is empty.
 */
export interface LegacyThreadCitation {
	id: string;
	source_type: CitationSourceType;
	/** `Cass. soc., 12 juill. 2022, n° 20-15.325` or the pièce's final name. */
	label: string;
	/** The exact supporting quote. A citation without one is not a citation. */
	quote: string;
	verification_state: CitationState;
	/** Article, alinéa, page range — where in the source the quote sits. */
	locator?: string;
	page_from?: number;
	page_to?: number;
	canonical_url?: string;
	/** Licence Ouverte 2.0 & co. — attribution is a product requirement (§13). */
	license_code?: string;
	license_notice?: string;
	/** The matter date retrieval ran against, in unix seconds. */
	as_of_date?: number | null;
	retrieved_at?: number;
	verified_by?: string;
	verified_at?: number | null;
}

/** New agent citations retain their discriminator; historical rows keep stored identity. */
export type ThreadCitation = AgentCitation | LegacyThreadCitation;

export function isAgentCitation(citation: ThreadCitation): citation is AgentCitation {
	return 'number' in citation && typeof citation.locator === 'object';
}

/** A clarification set answered in one go; a skip travels as an answer (§4.4). */
export type ClarificationAnswers = QuestionAnswer[];

/** Semantic tone for a step state — token names, never a colour. */
export type StepTone = 'muted' | 'primary' | 'amber' | 'pos' | 'destructive';

export const STEP_TONE: Record<StepState | string, StepTone> = {
	queued: 'muted',
	running: 'primary',
	waiting_input: 'amber',
	succeeded: 'pos',
	failed: 'destructive',
	cancelled: 'muted',
	skipped: 'muted'
};
