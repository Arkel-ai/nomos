import { derived, get, writable, type Readable } from 'svelte/store';
import {
	getConversation,
	postMessage,
	updateConversation,
	subscribeConversation,
	type Conversation,
	type ConversationHandlers,
	type Message,
	type MessageCreateRequest,
	type Transport
} from '$lib/api/conversations';
import {
	answerRun,
	cancelRun,
	getRun,
	getRunEvents,
	isTerminalRunState,
	type ApiError,
	type Artifact,
	type QuestionAnswer,
	type RunDetail,
	type RunEvent,
	type RunQuestion,
	type RunState,
	type RunSummary,
	type StepKind,
	type StepState,
	type WorkflowType
} from '$lib/api/runs';
import {
	AGENT_ACTIVITY_EVENT,
	type AgentActivityPayload,
	type AgentCitation,
	type CoverageReport
} from '$lib/api/agent-contracts';
import { listRunCitations, getRunCoverage } from '$lib/api/citations';
import { renameDossier } from './dossier';

/**
 * The thread's client-side state: the ordered message list plus the run/step
 * state derived from the durable event log (docs/PLAN.md §4.5b, §11).
 *
 * ONE REDUCER, TWO TRANSPORTS. `applyEvent` is the only way run state changes,
 * and it is fed identically by the SSE stream and by the cursor poll, so no
 * component knows or cares which is live.
 *
 * IDEMPOTENT BY EVENT ID. The SSE endpoint replays persisted events after the
 * cursor on every reconnect, so the same event id arrives more than once by
 * design. Applying one twice would duplicate a token delta or a step; the
 * `applied` set is what makes a replay free.
 */

// ---------------------------------------------------------------------------
// View shapes
// ---------------------------------------------------------------------------

/** One step as the plan monitor draws it (`○ ◐ ✔`). */
export interface StepView {
	id: string;
	step_key: string;
	kind: StepKind | string;
	label: string;
	state: StepState;
	/** Visible when a step retried — an attempt count is not a detail here. */
	attempt: number;
	max_attempts: number;
	/** 0–1 when the step reports fractional progress; undefined otherwise. */
	progress?: number;
	detail?: string;
	error_code?: string;
	error_summary?: string;
	/** Parallel fan-out is drawn from these edges, never guessed from order. */
	depends_on: string[];
	started_at?: number;
	finished_at?: number;
}

/** One mission, as it renders inside an assistant turn. */
export interface RunView {
	id: string;
	state: RunState;
	workflow_type: WorkflowType | string;
	workflow_version: number;
	steps: StepView[];
	questions: RunQuestion[];
	artifacts: Artifact[];
	failure_code?: string;
	/** True once the plan has been selected and parameterised. */
	planned: boolean;
	created_at: number;
	cancel_requested: boolean;

	// Agent runs (workflow_type nomos_agent) only.
	/** One row per call, grouped by dispatch; rows sharing a call_id replace each other. */
	activity?: (AgentActivityPayload & { retried?: boolean })[];
	/** First-use order, paged by 50. */
	citations?: AgentCitation[];
	citations_next_after?: number | null;
	citations_total?: number;
	citations_loading?: boolean;
	citations_error?: boolean;
	/** Server-derived from read receipts; null when unavailable. */
	coverage?: CoverageReport | null;
}

export interface ConversationState {
	id: string | null;
	loading: boolean;
	/** A terminal problem worth showing. A reconnect is NOT one. */
	error: string | null;
	conversation: Conversation | null;
	messages: Message[];
	runs: RunView[];
	/** Thread-level artifacts, deduplicated with the per-run lists. */
	artifacts: Artifact[];
	/** The durable event id everything is caught up to. */
	cursor: number;
	transport: Transport | null;
	connected: boolean;
	/** Live view lost, work continues. Show "reconnexion…", never an error. */
	reconnecting: boolean;
	sending: boolean;
}

function emptyState(): ConversationState {
	return {
		id: null,
		loading: false,
		error: null,
		conversation: null,
		messages: [],
		runs: [],
		artifacts: [],
		cursor: 0,
		transport: null,
		connected: false,
		reconnecting: false,
		sending: false
	};
}

// ---------------------------------------------------------------------------
// Reducer helpers
// ---------------------------------------------------------------------------

function runViewFrom(summary: RunSummary): RunView {
	return {
		id: summary.id,
		state: summary.state,
		workflow_type: summary.workflow_type,
		workflow_version: summary.workflow_version,
		steps: [],
		activity: [],
		questions: [],
		artifacts: [],
		planned: summary.steps_total > 0,
		created_at: summary.created_at,
		cancel_requested: false
	};
}

/**
 * The same view, rebuilt from GET /runs/:id.
 *
 * A thread read returns its runs as SUMMARIES — a state and two counts — and
 * everything the monitor draws (the steps, their edges, the clarification, the
 * artifacts) reaches the client as events. That works while the events are
 * still ahead of the cursor. It does not survive a reload: the thread comes
 * back with the cursor at the head, the stream replays nothing, and a finished
 * mission renders as an empty box under "Terminée" — with, worse, a parked
 * mission's questions gone, leaving the lawyer nothing to answer and the run
 * waiting for input for ever.
 *
 * So a reload rebuilds each run from its own rows. The steps are the same
 * shape the plan_ready payload carries, `depends_on` included — edges are step
 * ids on both paths, so a later event patches a hydrated step by id and the
 * reducer needs no special case.
 */
function runViewFromDetail(detail: RunDetail): RunView {
	const edges = new Map<string, string[]>();
	for (const dep of detail.deps ?? []) {
		edges.set(dep.step_id, [...(edges.get(dep.step_id) ?? []), dep.depends_on_step_id]);
	}
	return {
		id: detail.id,
		state: detail.state,
		workflow_type: detail.workflow_type,
		workflow_version: detail.workflow_version,
		steps: (detail.steps ?? []).map((s) => ({
			id: s.id,
			step_key: s.step_key,
			kind: s.kind,
			label: s.label || s.step_key,
			state: s.state,
			attempt: s.attempt_count,
			max_attempts: s.max_attempts,
			error_code: s.error_code,
			error_summary: s.error_summary,
			depends_on: edges.get(s.id) ?? [],
			started_at: s.started_at ?? undefined,
			finished_at: s.finished_at ?? undefined
		})),
		activity: [],
		questions: detail.questions ?? [],
		artifacts: detail.artifacts ?? [],
		failure_code: detail.failure_code || undefined,
		planned: (detail.steps ?? []).length > 0,
		created_at: detail.created_at,
		cancel_requested: detail.cancel_requested_at !== null
	};
}

/** Finds a run, creating a placeholder when an event outruns the thread read. */
function ensureRun(state: ConversationState, runId: string): RunView {
	const existing = state.runs.find((r) => r.id === runId);
	if (existing) return existing;
	const created: RunView = {
		id: runId,
		state: 'pending',
		workflow_type: '',
		workflow_version: 1,
		steps: [],
		activity: [],
		questions: [],
		artifacts: [],
		planned: false,
		created_at: Math.floor(Date.now() / 1000),
		cancel_requested: false
	};
	state.runs = [...state.runs, created];
	return created;
}

function upsertStep(run: RunView, patch: Partial<StepView> & { id: string }): void {
	const index = run.steps.findIndex((s) => s.id === patch.id);
	if (index === -1) {
		run.steps = [
			...run.steps,
			{
				id: patch.id,
				step_key: patch.step_key ?? patch.id,
				kind: patch.kind ?? 'db',
				label: patch.label ?? patch.step_key ?? '',
				state: patch.state ?? 'queued',
				attempt: patch.attempt ?? 0,
				max_attempts: patch.max_attempts ?? 3,
				progress: patch.progress,
				detail: patch.detail,
				error_code: patch.error_code,
				error_summary: patch.error_summary,
				depends_on: patch.depends_on ?? [],
				started_at: patch.started_at,
				finished_at: patch.finished_at
			}
		];
		return;
	}
	const next = [...run.steps];
	next[index] = { ...next[index], ...patch };
	run.steps = next;
}

function replaceRun(state: ConversationState, run: RunView): void {
	state.runs = state.runs.map((r) => (r.id === run.id ? { ...run } : r));
}

function upsertArtifact(list: Artifact[], artifact: Artifact): Artifact[] {
	return list.some((a) => a.id === artifact.id) ? list : [...list, artifact];
}

/**
 * The whole reducer. Returns the next state; the caller decides whether the
 * event had already been applied.
 */
export function reduceEvent(state: ConversationState, event: RunEvent): ConversationState {
	const next: ConversationState = { ...state, cursor: Math.max(state.cursor, event.id) };
	const runId = event.run_id;

	switch (event.type) {
		case AGENT_ACTIVITY_EVENT: {
			// One row per call: `running` first, then ok/empty/error under the
			// same call_id. Only agent runs emit these.
			if (!runId) break;
			const run = ensureRun(next, runId);
			run.workflow_type = 'nomos_agent';
			const rows = run.activity ?? [];
			const index = rows.findIndex((row) => row.call_id === event.payload.call_id);
			const previous = index === -1 ? undefined : rows[index];
			const retried =
				!!previous?.retried || (previous?.status === 'error' && event.payload.status !== 'error');
			const row = { ...event.payload, retried };
			const updated = index === -1 ? [...rows, row] : rows.map((r, i) => (i === index ? row : r));
			// A stable sort keeps the calls of one dispatch in arrival order.
			run.activity = updated.sort((a, b) => a.dispatch - b.dispatch);
			replaceRun(next, run);
			break;
		}

		case 'run.created': {
			if (!runId) break;
			const run = ensureRun(next, runId);
			run.workflow_type = event.payload.workflow_type ?? run.workflow_type;
			run.workflow_version = event.payload.workflow_version ?? run.workflow_version;
			replaceRun(next, run);
			// Bind the assistant turn to its mission so the monitor renders
			// inside the right message container.
			const messageId = event.payload.message_id;
			if (messageId) {
				next.messages = next.messages.map((m) =>
					m.id === messageId ? { ...m, run_id: runId } : m
				);
			}
			break;
		}

		case 'run.state_changed': {
			if (!runId) break;
			const run = ensureRun(next, runId);
			run.state = event.payload.state;
			run.failure_code = event.payload.failure_code || run.failure_code;
			replaceRun(next, run);
			break;
		}

		case 'run.plan_ready': {
			if (!runId) break;
			const run = ensureRun(next, runId);
			run.workflow_type = event.payload.workflow_type ?? run.workflow_type;
			run.workflow_version = event.payload.workflow_version ?? run.workflow_version;
			run.planned = true;
			// Same guard as question.asked: an event written before the plan
			// travelled whole carried `steps` as a COUNT, and mapping over a
			// number throws inside the reducer — taking every later event in
			// the batch down with it.
			const planned = Array.isArray(event.payload.steps) ? event.payload.steps : [];
			run.steps = planned.map((s) => ({
				id: s.id,
				step_key: s.step_key,
				kind: s.kind,
				label: s.label ?? s.step_key,
				state: s.state ?? 'queued',
				attempt: 0,
				max_attempts: 3,
				depends_on: s.depends_on ?? []
			}));
			replaceRun(next, run);
			break;
		}

		case 'step.started':
		case 'step.progress':
		case 'step.succeeded':
		case 'step.failed':
		case 'step.retrying': {
			if (!runId) break;
			const run = ensureRun(next, runId);
			const fallbackState: StepState =
				event.type === 'step.succeeded'
					? 'succeeded'
					: event.type === 'step.failed'
						? 'failed'
						: event.type === 'step.retrying'
							? 'queued'
							: 'running';
			upsertStep(run, {
				id: event.payload.step_id,
				step_key: event.payload.step_key,
				kind: event.payload.kind,
				label: event.payload.label,
				state: event.payload.state ?? fallbackState,
				attempt: event.payload.attempt,
				max_attempts: event.payload.max_attempts,
				progress: event.payload.progress,
				detail: event.payload.detail,
				error_code: event.payload.error_code,
				error_summary: event.payload.error_summary,
				started_at: event.type === 'step.started' ? event.created_at : undefined,
				finished_at:
					event.type === 'step.succeeded' || event.type === 'step.failed'
						? event.created_at
						: undefined
			});
			replaceRun(next, run);
			break;
		}

		case 'question.asked': {
			if (!runId) break;
			const run = ensureRun(next, runId);
			// Guard against an event whose payload predates questions being
			// carried whole (it listed bare keys): an entry with no
			// question_key would collapse the whole map onto one blank card.
			const incoming = (event.payload.questions ?? []).filter((q) => q?.question_key);
			const byKey = new Map(run.questions.map((q) => [q.question_key, q]));
			for (const q of incoming) byKey.set(q.question_key, q);
			run.questions = [...byKey.values()];
			replaceRun(next, run);
			break;
		}

		case 'question.answered': {
			if (!runId) break;
			const run = ensureRun(next, runId);
			run.questions = run.questions.map((q) =>
				q.question_key === event.payload.question_key
					? {
							...q,
							// A skip is an answer, never silence (§4.4).
							state: event.payload.skipped ? 'skipped' : 'answered',
							answered_by: event.payload.answered_by ?? q.answered_by,
							answered_at: event.created_at
						}
					: q
			);
			replaceRun(next, run);
			break;
		}

		case 'artifact.ready': {
			const artifact: Artifact = {
				id: event.payload.artifact_id,
				tenant_id: event.tenant_id,
				run_id: runId ?? '',
				dossier_id: '',
				kind: event.payload.kind,
				asset_id: '',
				name: event.payload.name,
				char_count: event.payload.char_count ?? 0,
				template_version: '',
				renderer_version: '',
				document_revision_id: event.payload.document_revision_id,
				verification_id: event.payload.verification_id,
				partial: event.payload.partial,
				piece_warnings: event.payload.piece_warnings,
				approved: event.payload.approved ?? false,
				approved_by: '',
				approved_at: null,
				created_at: event.created_at
			};
			next.artifacts = upsertArtifact(next.artifacts, artifact);
			if (runId) {
				const run = ensureRun(next, runId);
				run.artifacts = upsertArtifact(run.artifacts, artifact);
				replaceRun(next, run);
			}
			break;
		}

		case 'message.delta': {
			const { message_id, delta } = event.payload;
			const index = next.messages.findIndex((m) => m.id === message_id);
			if (index === -1) {
				// The stream outran the thread read; open the turn here.
				next.messages = [
					...next.messages,
					{
						id: message_id,
						tenant_id: event.tenant_id,
						conversation_id: event.conversation_id ?? next.id ?? '',
						role: 'assistant',
						content: delta,
						run_id: runId,
						state: 'streaming',
						created_at: event.created_at,
						updated_at: event.created_at
					}
				];
				break;
			}
			const messages = [...next.messages];
			messages[index] = { ...messages[index], content: messages[index].content + delta };
			next.messages = messages;
			break;
		}

		case 'conversation.titled': {
			if (next.conversation) {
				next.conversation = { ...next.conversation, title: event.payload.title };
			}
			break;
		}

		case 'attachments.read': {
			const { message_id } = event.payload;
			next.messages = next.messages.map((m) =>
				m.id === message_id && m.state === 'reading' ? { ...m, state: 'streaming' } : m
			);
			break;
		}

		case 'message.complete': {
			const { message_id, content, state: messageState, truncated, run_id } = event.payload;
			next.messages = next.messages.map((m) =>
				m.id === message_id
					? {
							...m,
							// The persisted text wins: a client that missed deltas
							// must still land on exactly what was stored.
							content: content ?? m.content,
							state: messageState ?? 'complete',
							truncated: truncated ?? m.truncated,
							agent_outcome: event.payload.agent_outcome ?? m.agent_outcome,
							answer_revision_id: event.payload.answer_revision_id ?? m.answer_revision_id,
							verification_id: event.payload.verification_id ?? m.verification_id,
							// A turn that waited on its attachments learns its
							// mission here, not from the POST that sent it.
							run_id: run_id ?? m.run_id,
							updated_at: event.created_at
						}
					: m
			);
			break;
		}

		case 'message.failed': {
			// Ends the turn: without it the reply stays `streaming` (caret
			// blinking, last word held back) until a reload reads the row.
			const { message_id, content, agent_outcome } = event.payload;
			next.messages = next.messages.map((m) =>
				m.id === message_id
					? {
							...m,
							// What had landed stays; an agent turn sends its final text.
							content: content ?? m.content,
							state: 'failed',
							agent_outcome: agent_outcome ?? m.agent_outcome,
							updated_at: event.created_at
						}
					: m
			);
			break;
		}
	}

	return next;
}

/** The calls whose text lands in the read-receipt ledger. */
const READING_TOOLS = new Set<string>(['read_piece', 'read_evidence']);

/**
 * True when `event` opens a dispatch that follows one which read pièces.
 * The server counts that text as read only once the next dispatch is
 * accepted — the moment that dispatch's first `running` row arrives — so
 * this is when the coverage card can move.
 */
function opensAfterReading(run: RunView, event: RunEvent): boolean {
	if (event.type !== AGENT_ACTIVITY_EVENT || event.payload.status !== 'running') return false;
	const { dispatch, call_id } = event.payload;
	const rows = run.activity ?? [];
	if (rows.some((row) => row.dispatch === dispatch && row.call_id !== call_id)) return false;
	const previous = Math.max(0, ...rows.filter((row) => row.dispatch < dispatch).map((row) => row.dispatch));
	return rows.some((row) => row.dispatch === previous && READING_TOOLS.has(row.tool));
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

export function createConversationStore() {
	const store = writable<ConversationState>(emptyState());
	/** Event ids already reduced — this is what makes a replay free. */
	let applied = new Set<number>();
	let controller: AbortController | null = null;

	function reset() {
		controller?.abort();
		controller = null;
		applied = new Set<number>();
		store.set(emptyState());
	}

	/**
	 * Applies one event exactly once. Safe to call from either transport.
	 *
	 * `history` marks an agent.activity event read back from the run's log on
	 * reload: it may lie past the thread snapshot, so it must not move the
	 * cursor — connect() still has to replay whatever else lies in between.
	 */
	function applyEvent(event: RunEvent, history = false) {
		if (applied.has(event.id)) return;
		applied.add(event.id);
		store.update((s) => {
			const next = reduceEvent(s, event);
			return history ? { ...next, cursor: s.cursor } : next;
		});
		// An agent answer or its end brings sources, coverage and published
		// artifacts with it; none of them travel in the events themselves.
		// Coverage also follows the reading while the turn runs.
		if (!history && event.run_id) {
			const run = get(store).runs.find((r) => r.id === event.run_id);
			if (run?.workflow_type === 'nomos_agent') {
				const ended = event.type === 'run.state_changed' && isTerminalRunState(run.state);
				if (ended || event.type === 'message.complete' || opensAfterReading(run, event)) {
					void refreshAgentSources(run.id);
				}
				if (ended) void reloadRun(run.id).catch(() => {});
			}
		}
		// The dossier lives in its own store; the thread only relays its name.
		if (event.type === 'dossier.titled') {
			renameDossier(event.payload.dossier_id, event.payload.title);
		}
	}

	function applyEvents(events: RunEvent[]) {
		for (const event of events) applyEvent(event);
	}

	/**
	 * GET the thread, then subscribe from the cursor it came back with.
	 *
	 * Resolves false when the thread does not exist — deleted, or a stale tab
	 * or link. The store is reset then, so nothing (a bind, a send) can keep
	 * aiming at an id the API will only ever answer 404 for.
	 */
	async function load(id: string): Promise<boolean> {
		const current = get(store);
		if (current.id !== id) {
			controller?.abort();
			controller = null;
			applied = new Set<number>();
			store.set({ ...emptyState(), id, loading: true });
		} else {
			store.update((s) => ({ ...s, loading: true, error: null }));
		}

		try {
			const detail = await getConversation(id);
			if (get(store).id !== id) return true;
			applied = new Set<number>();
			store.update((s) => ({
				...s,
				id,
				loading: false,
				error: null,
				conversation: {
					id: detail.id,
					tenant_id: detail.tenant_id,
					dossier_id: detail.dossier_id,
					title: detail.title,
					created_by: detail.created_by,
					state: detail.state,
					created_at: detail.created_at,
					updated_at: detail.updated_at
				},
				messages: detail.messages ?? [],
				runs: (detail.runs ?? []).map(runViewFrom),
				artifacts: detail.artifacts ?? [],
				cursor: Math.max(s.cursor, detail.cursor ?? 0)
			}));
			await hydrateRuns(id, detail.runs ?? []);
			return true;
		} catch (err) {
			if ((err as { status?: number })?.status === 404) {
				reset();
				return false;
			}
			const message = (err as { message?: string })?.message ?? 'errors.loadFailed';
			store.update((s) => ({ ...s, loading: false, error: message }));
			return true;
		}
	}

	/**
	 * Rebuilds each run from its own rows, so a reloaded thread draws its plan,
	 * its clarification and its artifacts instead of an empty box. It runs
	 * BEFORE connect() opens the stream: the cursor was taken at the thread
	 * read, so anything that happened since replays on top of what is
	 * hydrated here rather than being lost behind it.
	 *
	 * A run that cannot be read is left as its summary — a thread that renders
	 * one mission thinly is better than a thread that fails to render.
	 */
	async function hydrateRuns(id: string, summaries: RunSummary[]): Promise<void> {
		if (!summaries.length) return;
		const details = await Promise.all(
			summaries.map((summary) => getRun(summary.id).catch(() => null))
		);
		store.update((s) => {
			if (s.id !== id) return s;
			const byID = new Map(
				details.filter((d): d is RunDetail => d !== null).map((d) => [d.id, d])
			);
			if (!byID.size) return s;
			let artifacts = s.artifacts;
			for (const detail of byID.values()) {
				for (const artifact of detail.artifacts ?? []) {
					artifacts = upsertArtifact(artifacts, artifact);
				}
			}
			return {
				...s,
				artifacts,
				runs: s.runs.map((run) => {
					const detail = byID.get(run.id);
					return detail ? runViewFromDetail(detail) : run;
				})
			};
		});
		const agentRuns = summaries.filter((run) => run.workflow_type === 'nomos_agent');
		await Promise.all(
			agentRuns.flatMap((run) => [refreshAgentSources(run.id), hydrateActivity(id, run.id)])
		);
	}

	/**
	 * Rebuilds an agent run's activity from its durable `agent.activity`
	 * events (owner decision 1). Only those are applied: the thread read
	 * already holds the stored answer, and replaying message.delta would
	 * duplicate it. A history that cannot be read leaves the activity empty —
	 * it never hides the answer.
	 */
	async function hydrateActivity(id: string, runId: string): Promise<void> {
		const PAGE = 500;
		try {
			const history: RunEvent[] = [];
			let after = 0;
			for (;;) {
				const page = await getRunEvents(runId, after, PAGE);
				const events = page.events ?? [];
				history.push(...events.filter((event) => event.type === AGENT_ACTIVITY_EVENT));
				if (events.length < PAGE || page.next_after <= after) break;
				after = page.next_after;
			}
			if (get(store).id !== id) return;
			for (const event of history) applyEvent(event, true);
		} catch {
			/* the activity stays empty */
		}
	}

	/**
	 * Reads an agent run's citations (first page, or the next one with
	 * `more`) and its coverage. Both routes may be absent from this build:
	 * 404/501 renders nothing rather than an error. Only the latest read per
	 * run lands, so a slow early read cannot overwrite the terminal one.
	 */
	const sourceReads = new Map<string, symbol>();
	async function refreshAgentSources(runId: string, more = false) {
		const id = get(store).id;
		const run = get(store).runs.find((r) => r.id === runId);
		if (!run) return;
		if (more && (run.citations_loading || run.citations_next_after == null)) return;
		const token = Symbol();
		sourceReads.set(runId, token);
		patchRun(runId, { citations_loading: true, citations_error: false });

		const [citations, coverage] = await Promise.allSettled([
			listRunCitations(runId, {
				limit: 50,
				after: more ? (run.citations_next_after ?? undefined) : undefined
			}),
			more ? Promise.resolve(run.coverage ?? null) : getRunCoverage(runId)
		]);
		if (get(store).id !== id || sourceReads.get(runId) !== token) return;

		const current = get(store).runs.find((r) => r.id === runId);
		if (!current) return;
		const patch: Partial<RunView> = {
			citations_loading: false,
			coverage: coverage.status === 'fulfilled' ? coverage.value : null
		};
		if (citations.status === 'fulfilled') {
			const merged = new Map((more ? (current.citations ?? []) : []).map((c) => [c.id, c]));
			for (const item of citations.value.items ?? []) merged.set(item.id, item);
			patch.citations = [...merged.values()].sort((a, b) => a.number - b.number);
			patch.citations_next_after = citations.value.next_after;
			patch.citations_total = citations.value.total;
		} else if ([404, 501].includes((citations.reason as ApiError | undefined)?.status ?? 0)) {
			patch.citations = [];
			patch.citations_next_after = null;
			patch.citations_total = 0;
		} else {
			patch.citations_error = true;
		}
		patchRun(runId, patch);
	}

	function patchRun(runId: string, patch: Partial<RunView>) {
		store.update((s) => ({
			...s,
			runs: s.runs.map((r) => (r.id === runId ? { ...r, ...patch } : r))
		}));
	}

	/**
	 * Re-reads one run from its rows: after an agent turn ends (its published
	 * artifacts carry fields artifact.ready does not), after a stale answer,
	 * or to learn the turn that holds the thread. Keeps the activity, sources
	 * and coverage already shown, and never reopens a turn the stream has
	 * already seen end.
	 */
	const runReads = new Map<string, symbol>();
	async function reloadRun(runId: string) {
		const id = get(store).id;
		const token = Symbol();
		runReads.set(runId, token);
		const detail = await getRun(runId);
		store.update((s) => {
			if (s.id !== id || runReads.get(runId) !== token) return s;
			const current = s.runs.find((run) => run.id === runId);
			if (current && isTerminalRunState(current.state) && !isTerminalRunState(detail.state)) {
				return s;
			}
			const fresh = runViewFromDetail(detail);
			return {
				...s,
				runs: current
					? s.runs.map((run) =>
							run.id === runId ? { ...run, ...fresh, activity: run.activity } : run
						)
					: [...s.runs, fresh],
				artifacts: s.artifacts.map(
					(artifact) => detail.artifacts?.find((a) => a.id === artifact.id) ?? artifact
				)
			};
		});
		return detail;
	}

	/**
	 * Opens the live view. Prefers SSE, falls back to the cursor poll, and
	 * treats a dropped connection as a reconnect — never as a failed run.
	 * Returns a stop function; call it on unmount.
	 */
	function connect(id: string): () => void {
		controller?.abort();
		const own = new AbortController();
		controller = own;

		const handlers: ConversationHandlers = {
			onEvent: applyEvent,
			onCursor: (after) =>
				store.update((s) => ({ ...s, cursor: Math.max(s.cursor, after) })),
			onOpen: (transport) =>
				store.update((s) => ({ ...s, transport, connected: true, reconnecting: false })),
			onReconnect: () =>
				store.update((s) => ({ ...s, connected: false, reconnecting: true })),
			onFatal: (error) =>
				store.update((s) => ({
					...s,
					connected: false,
					reconnecting: false,
					error: error.message
				}))
		};

		void subscribeConversation(id, get(store).cursor, handlers, own.signal).finally(() => {
			if (controller === own) {
				store.update((s) => ({ ...s, connected: false, reconnecting: false }));
			}
		});

		return () => {
			own.abort();
			if (controller === own) controller = null;
		};
	}

	/**
	 * Sends a turn and folds the durable rows the API returns straight into the
	 * thread, so the composer clears against real ids rather than a guess.
	 */
	async function send(id: string, body: MessageCreateRequest) {
		store.update((s) => ({ ...s, sending: true, error: null }));
		try {
			const result = await postMessage(id, body);
			store.update((s) => {
				const known = new Set(s.messages.map((m) => m.id));
				const added = [result.user_message, result.assistant_message].filter(
					(m) => m && !known.has(m.id)
				);
				return {
					...s,
					sending: false,
					messages: [...s.messages, ...added],
					cursor: Math.max(s.cursor, result.cursor ?? 0)
				};
			});
			// The turn holds the thread from now on, before any event says so.
			const agent = result.agent;
			if (agent) {
				store.update((s) => {
					const next = { ...s };
					const run = ensureRun(next, agent.run_id);
					run.workflow_type = 'nomos_agent';
					replaceRun(next, run);
					return next;
				});
				void reloadRun(agent.run_id).catch(() => {});
			}
			return result;
		} catch (err) {
			const message = (err as { message?: string })?.message ?? 'errors.saveFailed';
			store.update((s) => ({ ...s, sending: false, error: message }));
			throw err;
		}
	}

	/** Answers a clarification set. A skip travels here as an answer too. */
	async function answer(runId: string, answers: QuestionAnswer[]) {
		const detail = await answerRun(runId, answers);
		store.update((s) => {
			const run = s.runs.find((r) => r.id === runId);
			if (!run) return s;
			const updated: RunView = { ...run, questions: detail.questions ?? run.questions };
			return { ...s, runs: s.runs.map((r) => (r.id === runId ? updated : r)) };
		});
		return detail;
	}

	/** Binds the thread to a dossier collection, so attachments have a home. */
	async function bind(dossierId: string) {
		const id = get(store).id;
		if (!id) return;
		const updated = await updateConversation(id, { dossier_id: dossierId });
		store.update((s) => ({
			...s,
			conversation: s.conversation ? { ...s.conversation, ...updated } : updated
		}));
	}

	/** Requests cancellation — observed at the next safe point, not a kill. */
	async function cancel(runId: string) {
		const result = await cancelRun(runId);
		store.update((s) => ({
			...s,
			runs: s.runs.map((r) =>
				r.id === runId ? { ...r, state: result.state, cancel_requested: true } : r
			)
		}));
		return result;
	}

	return {
		subscribe: store.subscribe,
		load,
		connect,
		send,
		answer,
		reloadRun,
		refreshAgentSources,
		cancel,
		bind,
		applyEvent,
		applyEvents,
		reset
	};
}

/** The thread the app is currently showing. */
export const conversation = createConversationStore();

/** The run still doing something, if any — what the plan monitor animates. */
export const activeRun: Readable<RunView | null> = derived(conversation, ($c) => {
	for (let i = $c.runs.length - 1; i >= 0; i--) {
		if (!isTerminalRunState($c.runs[i].state)) return $c.runs[i];
	}
	return null;
});

/** Every clarification still waiting on the lawyer, across the thread. */
export const pendingQuestions: Readable<{ run: RunView; questions: RunQuestion[] }[]> = derived(
	conversation,
	($c) =>
		$c.runs
			.map((run) => ({ run, questions: run.questions.filter((q) => q.state === 'pending') }))
			.filter((entry) => entry.questions.length > 0)
);

/** True while the live view is degraded but the work is unaffected. */
export const liveViewDegraded: Readable<boolean> = derived(
	conversation,
	($c) => $c.reconnecting || (!$c.connected && $c.id !== null && !$c.loading)
);
