import type { OrbState } from '$lib/components/libraries/thinking-orb';
import type { RunView, StepView } from '$lib/stores/conversation';
import { isTerminalRunState } from '$lib/api/runs';

export type Activity = { state: OrbState; tone: 'brand' | 'amber' | 'idle' };

// Step type → the orb's figure. Matched on the step key's prefix (the
// workflow's step type); the worker pool is the fallback for anything a
// newer server invents.
const BY_STEP: [prefix: string, state: OrbState][] = [
	['extract_text', 'weaving'],
	['ocr_page', 'weaving'],
	['ocr_batch', 'weaving'],
	['chunk_and_embed', 'connecting'],
	['resolve_citations', 'connecting'],
	['qualify_piece', 'solving'],
	['build_classement', 'solving'],
	['search_law', 'searching'],
	['search_caselaw', 'searching'],
	['synthesize', 'composing'],
	['draft_document', 'composing'],
	['render_docx', 'composing'],
	['export_dossier', 'composing']
];

const BY_POOL: Record<string, OrbState> = {
	ocr: 'weaving',
	render: 'composing',
	llm: 'working',
	db: 'solving'
};

function stepOrb(step: StepView): OrbState {
	const key = step.step_key ?? '';
	return BY_STEP.find(([prefix]) => key.startsWith(prefix))?.[1] ?? BY_POOL[step.kind] ?? 'working';
}

/**
 * What the orb shows for a run: planning shapes, a waiting run breathes in
 * amber, a running step draws its own figure, a finished run goes still.
 */
export function runActivity(run: RunView): Activity {
	if (isTerminalRunState(run.state)) return { state: 'breathing', tone: 'idle' };
	if (run.state === 'waiting_input') return { state: 'breathing', tone: 'amber' };
	if (!run.planned && !run.steps.length) return { state: 'shaping', tone: 'brand' };
	const running = run.steps.find((s) => s.state === 'running');
	return { state: running ? stepOrb(running) : 'working', tone: 'brand' };
}

/** Public category only; tool names never appear in the interface. */
export function agentActivityKey(tool?: string): string {
	if (['list_pieces', 'search_pieces', 'read_piece', 'read_evidence'].includes(tool ?? '')) return 'chat.agent.reading';
	if (['search_law', 'read_law', 'search_caselaw', 'read_decision', 'search_web', 'list_competences', 'load_competence'].includes(tool ?? '')) return 'chat.agent.researching';
	if (['write_document', 'render_document', 'export_dossier', 'calculate'].includes(tool ?? '')) return 'chat.agent.drafting';
	if (['edit_document', 'read_document', 'review_output'].includes(tool ?? '')) return 'chat.agent.revising';
	if (['verify_output', 'submit_result'].includes(tool ?? '')) return 'chat.agent.verifying';
	return 'chat.agent.working';
}

/**
 * A duration as the thread shows it: "12,3 s" under a minute, then
 * "14 min 29 s", then "1 h 02 min" — never "869,1 s".
 */
export function formatElapsed(seconds: number, locale: string, tenths = true): string {
	if (seconds < 60) {
		const digits = tenths ? 1 : 0;
		return `${new Intl.NumberFormat(locale, { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(seconds)} s`;
	}
	const minutes = Math.floor(seconds / 60);
	if (minutes < 60) return `${minutes} min ${String(Math.floor(seconds % 60)).padStart(2, '0')} s`;
	return `${Math.floor(minutes / 60)} h ${String(minutes % 60).padStart(2, '0')} min`;
}
