import { fetchApi } from './fetchApi';
import { AGENT_ACTIVITY_EVENT, type AgentActivityPayload, type AgentOutcome } from './agent-contracts';

/**
 * Runs — the mission surface (docs/PLAN.md §10).
 *
 * A run is never navigated to on its own: its plan, its clarification cards
 * and its artifacts render as blocks inside an assistant turn (§11). These
 * calls exist so a thread can read the plan it is showing and answer the
 * questions it is asking.
 *
 * This module also owns the shapes the rest of the Nomos clients reference —
 * run steps, run questions, artifacts and, above all, the durable `RunEvent`
 * that both transports in `conversations.ts` replay.
 */

// ---------------------------------------------------------------------------
// Client plumbing
// ---------------------------------------------------------------------------

/** A non-2xx response, carrying the API's `detail` message. */
export interface ApiError {
	status: number;
	message: string;
	code?: string;
	run_id?: string;
}

/**
 * fetchApi resolves with `{ status, data }` and never throws on non-2xx, so
 * unwrap turns an error status into a throw and otherwise returns the typed
 * payload — the same convention as `api/files.ts`.
 */
export function unwrapRun<T>(res: { status: number; data: T }): T {
	if (res.status < 200 || res.status >= 300) {
		const body = res.data as { detail?: string; code?: string; run_id?: string } | null;
		throw { status: res.status, message: body?.detail ?? `Request failed (${res.status})`,
			code: body?.code, run_id: body?.run_id } as ApiError;
	}
	return res.data;
}

/**
 * Retry keys of the sends whose outcome is still unknown, by path and body. A send mints its `request_id` before its first
 * POST; resending the SAME body to the same path reuses it until an answer
 * says what happened, so the API returns what it already committed instead of
 * acting twice. Any other send gets a new id.
 */
const pendingRequestIds = new Map<string, string>();

function newRequestId(): string | undefined {
	try {
		return globalThis.crypto?.randomUUID?.();
	} catch {
		return undefined; // no secure context: the API mints one
	}
}

/**
 * POSTs a request that starts work, under its retry key. A network failure or
 * an unreadable answer leaves the outcome unknown, and so does a 5xx: the key
 * is kept for the retry. Any other answer settles the send.
 */
export async function postWithRequestId<T>(path: string, body: object): Promise<T> {
	const key = `${path}\n${JSON.stringify(body)}`;
	let requestId = pendingRequestIds.get(key);
	if (!requestId) {
		requestId = newRequestId();
		if (requestId) pendingRequestIds.set(key, requestId);
	}
	const res = await fetchApi<T>(path, 'POST', requestId ? { ...body, request_id: requestId } : body);
	if (res.status < 500) pendingRequestIds.delete(key);
	return unwrapRun(res);
}

/** Builds a `?a=1&b=2` suffix, dropping undefined/null/empty values. */
export function queryString(params: Record<string, string | number | boolean | undefined | null>) {
	const qs = new URLSearchParams();
	for (const [k, v] of Object.entries(params)) {
		if (v === undefined || v === null || v === '') continue;
		qs.set(k, String(v));
	}
	const s = qs.toString();
	return s ? `?${s}` : '';
}

/** The envelope every paginated Nomos list returns (types.ListResponse). */
export interface ListResponse<T> {
	items: T[];
	total: number;
	limit: number;
	offset: number;
}

/** Offset pagination accepted by every list endpoint. */
export interface ListQuery {
	limit?: number;
	offset?: number;
}

/** Presigned-URL envelope shared by piece and artifact downloads. */
export interface DownloadLink {
	url: string;
	name: string;
	expires_in: number;
}

// ---------------------------------------------------------------------------
// State machines (mirrors api/types/nomos_const.go)
// ---------------------------------------------------------------------------

export type RunState =
	| 'pending'
	| 'running'
	| 'waiting_input'
	| 'succeeded'
	| 'failed'
	| 'cancelled';

export type StepState =
	| 'queued'
	| 'running'
	| 'waiting_input'
	| 'succeeded'
	| 'failed'
	| 'cancelled'
	| 'skipped';

export type StepKind = 'db' | 'llm' | 'ocr' | 'render';

export type WorkflowType = 'dossier_organize_v1' | 'nomos_agent';

export type QuestionState = 'pending' | 'answered' | 'skipped';
export type QuestionKind = 'single' | 'multi' | 'text';

export type ArtifactKind = 'bordereau_docx' | 'export_zip' | 'note_docx';

/** A run in a terminal state will never change again. */
export function isTerminalRunState(s: RunState | string): boolean {
	return s === 'succeeded' || s === 'failed' || s === 'cancelled';
}

/** A step in a terminal state will never change again. */
export function isTerminalStepState(s: StepState | string): boolean {
	return s === 'succeeded' || s === 'failed' || s === 'cancelled' || s === 'skipped';
}

// ---------------------------------------------------------------------------
// Row shapes
// ---------------------------------------------------------------------------

export interface Run {
	id: string;
	tenant_id: string;
	conversation_id: string | null;
	dossier_id: string;
	requested_by: string;
	workflow_type: WorkflowType | string;
	workflow_version: number;
	instruction: string;
	state: RunState;
	cancel_requested_at: number | null;
	failure_code: string;
	created_at: number;
	updated_at: number;
	started_at: number | null;
	finished_at: number | null;
}

export interface RunStep {
	id: string;
	tenant_id: string;
	run_id: string;
	step_key: string;
	kind: StepKind | string;
	state: StepState;
	/** Postgres timestamptz — an ISO-8601 string on the wire. */
	available_at: string;
	attempt_count: number;
	max_attempts: number;
	idempotency_key: string;
	lease_owner: string;
	lease_expires_at: string | null;
	heartbeat_at: string | null;
	input?: Record<string, unknown>;
	output?: Record<string, unknown>;
	error_code: string;
	error_summary: string;
	/** Human label for the plan monitor; the workflow definition sets it. */
	label: string;
	created_at: number;
	updated_at: number;
	started_at: number | null;
	finished_at: number | null;
}

/** One dependency edge. Parallel fan-out is drawn from these, not guessed. */
export interface RunStepDep {
	id: string;
	run_id: string;
	step_id: string;
	depends_on_step_id: string;
	created_at: number;
}

export interface RunQuestion {
	id: string;
	tenant_id: string;
	run_id: string;
	run_step_id: string;
	question_key: string;
	prompt: string;
	kind: QuestionKind | string;
	options?: QuestionOption[];
	state: QuestionState;
	/** Agent questions: the work cannot finish without it; a skip limits it. */
	required?: boolean;
	answer?: Record<string, unknown>;
	answered_by: string;
	asked_at: number;
	answered_at: number | null;
}

export interface QuestionOption {
	value: string;
	label: string;
	hint?: string;
}

export interface Artifact {
	id: string;
	tenant_id: string;
	run_id: string;
	dossier_id: string;
	kind: ArtifactKind | string;
	asset_id: string;
	name: string;
	char_count: number;
	template_version: string;
	renderer_version: string;
	/** False means the artifact is stamped "projet — vérification de l'avocat requise". */
	approved: boolean;
	approved_by: string;
	approved_at: number | null;
	/** Agent artifacts only; null/absent on legacy artifacts. */
	document_revision_id?: string | null;
	verification_id?: string | null;
	/** Explicitly incomplete work. */
	partial?: boolean;
	/** Pièce details to check; download stays available. */
	piece_warnings?: number;
	created_at: number;
}

/** The compact run shape embedded in a conversation thread. */
export interface RunSummary {
	id: string;
	state: RunState;
	workflow_type: WorkflowType | string;
	workflow_version: number;
	steps_total: number;
	steps_done: number;
	created_at: number;
}

/** GET /runs/:id — the plan the monitor renders. */
export interface RunDetail extends Run {
	steps: RunStep[];
	deps: RunStepDep[];
	questions: RunQuestion[];
	artifacts: Artifact[];
	cursor: number;
}

// ---------------------------------------------------------------------------
// The durable event log (§4.5 / §4.5b)
// ---------------------------------------------------------------------------

/**
 * Event types, mirroring the Go constants in api/types/nomos_const.go. The
 * union is closed on purpose: an unknown type is ignored by the reducer rather
 * than rendered, so a newer server can never make an older client draw
 * something it does not understand.
 */
export type NomosEventType =
	| typeof AGENT_ACTIVITY_EVENT
	| 'run.created'
	| 'run.state_changed'
	| 'run.plan_ready'
	| 'step.started'
	| 'step.progress'
	| 'step.succeeded'
	| 'step.failed'
	| 'step.retrying'
	| 'question.asked'
	| 'question.answered'
	| 'artifact.ready'
	| 'message.delta'
	| 'message.complete'
	| 'message.failed'
	| 'attachments.read'
	| 'conversation.titled'
	| 'dossier.titled';

export interface RunCreatedPayload {
	workflow_type?: WorkflowType | string;
	workflow_version?: number;
	instruction?: string;
	dossier_id?: string;
	message_id?: string;
}

export interface RunStateChangedPayload {
	state: RunState;
	failure_code?: string;
	finished_at?: number;
}

/** The plan, as soon as the workflow has been selected and parameterised. */
export interface PlanReadyPayload {
	workflow_type?: WorkflowType | string;
	workflow_version?: number;
	steps: PlanStep[];
}

export interface PlanStep {
	id: string;
	step_key: string;
	kind: StepKind | string;
	label?: string;
	state?: StepState;
	depends_on?: string[];
}

/** Shared by every step.* event. `attempt` is visible so a retry shows. */
export interface StepPayload {
	step_id: string;
	step_key?: string;
	kind?: StepKind | string;
	label?: string;
	state?: StepState;
	attempt?: number;
	max_attempts?: number;
	/** 0–1 for a step that can report fractional progress (per-piece fan-out). */
	progress?: number;
	detail?: string;
	error_code?: string;
	error_summary?: string;
	/** Seconds until the step is retried, on step.retrying. */
	retry_in?: number;
}

export interface QuestionAskedPayload {
	step_id?: string;
	questions: RunQuestion[];
}

export interface QuestionAnsweredPayload {
	step_id?: string;
	question_key: string;
	skipped: boolean;
	answered_by?: string;
}

export interface ArtifactReadyPayload {
	document_revision_id?: string | null;
	verification_id?: string | null;
	partial?: boolean;
	piece_warnings?: number;
	artifact_id: string;
	kind: ArtifactKind | string;
	name: string;
	char_count?: number;
	approved?: boolean;
}

/** A conversational reply's token delta (§4.5b). Never a drafting step. */
export interface MessageDeltaPayload {
	message_id: string;
	delta: string;
}

export interface MessageCompletePayload {
	agent_outcome?: AgentOutcome;
	answer_revision_id?: string | null;
	verification_id?: string | null;
	message_id: string;
	/** The final persisted text, so a client that missed deltas still lands right. */
	content?: string;
	state?: 'complete' | 'failed';
	failure_code?: string;
	/** True when the reply stopped on the completion budget, not on its own. */
	truncated?: boolean;
	/** Set when a turn that waited on its attachments became a mission. */
	run_id?: string;
}

/**
 * An assistant turn that could not be produced. A conversational reply carries
 * only the code; an agent turn also carries its outcome and the final text
 * (whatever had landed plus the explanation).
 */
export interface MessageFailedPayload {
	message_id: string;
	run_id?: string;
	code?: string;
	agent_outcome?: AgentOutcome;
	content?: string;
}

/** A turn that waited on its attachments is answered next (§4.5b). */
export interface AttachmentsReadPayload {
	message_id: string;
	pieces: number;
	/** Still being read when the wait gave up; the turn is answered anyway. */
	unread: number;
}

/** The thread's generated title. */
export interface ConversationTitledPayload {
	title: string;
}

/** The generated name of the dossier the composer opened for the thread's files. */
export interface DossierTitledPayload {
	dossier_id: string;
	title: string;
}

/** The envelope every event arrives in. `id` is the cursor and the dedup key. */
interface RunEventBase {
	id: number;
	tenant_id: string;
	run_id: string | null;
	conversation_id: string | null;
	run_step_id: string | null;
	created_at: number;
}

export type RunEvent = RunEventBase &
	(
		| { type: typeof AGENT_ACTIVITY_EVENT; payload: AgentActivityPayload }
		| { type: 'run.created'; payload: RunCreatedPayload }
		| { type: 'run.state_changed'; payload: RunStateChangedPayload }
		| { type: 'run.plan_ready'; payload: PlanReadyPayload }
		| { type: 'step.started'; payload: StepPayload }
		| { type: 'step.progress'; payload: StepPayload }
		| { type: 'step.succeeded'; payload: StepPayload }
		| { type: 'step.failed'; payload: StepPayload }
		| { type: 'step.retrying'; payload: StepPayload }
		| { type: 'question.asked'; payload: QuestionAskedPayload }
		| { type: 'question.answered'; payload: QuestionAnsweredPayload }
		| { type: 'artifact.ready'; payload: ArtifactReadyPayload }
		| { type: 'message.delta'; payload: MessageDeltaPayload }
		| { type: 'message.complete'; payload: MessageCompletePayload }
		| { type: 'message.failed'; payload: MessageFailedPayload }
		| { type: 'attachments.read'; payload: AttachmentsReadPayload }
		| { type: 'conversation.titled'; payload: ConversationTitledPayload }
		| { type: 'dossier.titled'; payload: DossierTitledPayload }
	);

/** One cursor page. `next_after` equals `after` when nothing new arrived. */
export interface EventsResponse {
	events: RunEvent[];
	next_after: number;
}

/** True for an event type this build knows how to reduce. */
export function isKnownEventType(type: string): type is NomosEventType {
	return knownEventTypes.has(type);
}

const knownEventTypes = new Set<string>([
	AGENT_ACTIVITY_EVENT,
	'run.created',
	'run.state_changed',
	'run.plan_ready',
	'step.started',
	'step.progress',
	'step.succeeded',
	'step.failed',
	'step.retrying',
	'question.asked',
	'question.answered',
	'artifact.ready',
	'message.delta',
	'message.complete',
	'message.failed',
	'attachments.read',
	'conversation.titled',
	'dossier.titled'
]);

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

/** One answered — or deliberately skipped — clarification. */
export interface QuestionAnswer {
	question_key: string;
	values?: string[];
	text?: string;
	/** A skip is recorded as an answer, never as silence (§4.4). */
	skipped?: boolean;
}

export interface CancelResponse {
	run_id: string;
	state: RunState;
	cancel_requested_at: number;
}

// ---------------------------------------------------------------------------
// Calls
// ---------------------------------------------------------------------------

/** GET /runs/:id — plan, steps, dependency edges, questions and artifacts. */
export async function getRun(id: string): Promise<RunDetail> {
	return unwrapRun(await fetchApi<RunDetail>(`/runs/${id}`));
}

/**
 * GET /runs/:id/events — the same durable cursor a thread polls, scoped to one
 * run. `after` is a run_events.id, never a timestamp: that is what survives a
 * pod restart.
 */
export async function getRunEvents(
	id: string,
	after = 0,
	limit = 100
): Promise<EventsResponse> {
	return unwrapRun(
		await fetchApi<EventsResponse>(`/runs/${id}/events${queryString({ after, limit })}`)
	);
}

/** POST /runs/:id/answers — the HITL reply. Skips travel here too. */
export async function answerRun(id: string, answers: QuestionAnswer[]): Promise<RunDetail> {
	return unwrapRun(await fetchApi<RunDetail>(`/runs/${id}/answers`, 'POST', { answers }));
}

/**
 * POST /runs/:id/cancel — a request observed at the next safe point, not a
 * kill. The UI must say "interruption demandée" until the run reports back.
 */
export async function cancelRun(id: string): Promise<CancelResponse> {
	return unwrapRun(await fetchApi<CancelResponse>(`/runs/${id}/cancel`, 'POST'));
}

/** GET /runs/:id/artifacts */
export async function listRunArtifacts(id: string): Promise<Artifact[]> {
	return unwrapRun(await fetchApi<Artifact[]>(`/runs/${id}/artifacts`));
}

/** GET /artifacts/:id/download — a fresh presigned URL (the bucket is private). */
export async function getArtifactDownloadLink(id: string): Promise<DownloadLink> {
	return unwrapRun(await fetchApi<DownloadLink>(`/artifacts/${id}/download`));
}

/**
 * Download an artifact: ask for a signed URL on click, then let the browser
 * fetch it straight from S3. The signed URL is never stored or logged.
 */
export async function downloadArtifact(id: string): Promise<void> {
	const link = await getArtifactDownloadLink(id);
	const a = document.createElement('a');
	a.href = link.url;
	a.download = link.name;
	a.rel = 'noopener';
	document.body.appendChild(a);
	a.click();
	a.remove();
}
