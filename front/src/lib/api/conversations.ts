import { fetchApi } from './fetchApi';
import type { AgentIntakeErrorBody, AgentOutcome, AgentTurnStatus } from './agent-contracts';
import {
	isKnownEventType,
	postWithRequestId,
	queryString,
	unwrapRun as unwrap,
	type ApiError,
	type Artifact,
	type EventsResponse,
	type ListQuery,
	type ListResponse,
	type RunEvent,
	type RunSummary
} from './runs';

/**
 * Conversations — the product's primary entry point (docs/PLAN.md §10, §11).
 *
 * A thread carries plain conversational turns *and* long-running missions,
 * interleaved. Two transports read the SAME durable cursor (§4.5b):
 *
 *   streamConversation — SSE. Replays persisted events after the cursor, then
 *                        goes live. A dropped connection is a reconnect, never
 *                        a failed run: the work is durable, only the view was
 *                        lost.
 *   pollConversation   — the 1–2 s cursor poll over GET …/events?after=, for a
 *                        client (or a build) that cannot hold a connection.
 *
 * Both emit the same `RunEvent`s in the same order, so the reducer in
 * `$lib/stores/conversation` — and therefore every component — does not care
 * which one is live.
 *
 * The transports use `fetch` directly rather than `fetchApi`: they need a
 * streaming body and an AbortSignal, which the shared wrapper does not model.
 * Everything else about them is identical — the same relative `/api` path and
 * `credentials: 'include'`, never a hand-attached Authorization header.
 */

const API_BASE = '/api';

// ---------------------------------------------------------------------------
// Rows and requests
// ---------------------------------------------------------------------------

export type ConversationState = 'active' | 'archived';
export type MessageRole = 'user' | 'assistant' | 'system';
/** `reading`: the turn waits for its attachments to be ingested, then is answered. */
export type MessageState = 'reading' | 'streaming' | 'complete' | 'failed';

export interface Conversation {
	id: string;
	tenant_id: string;
	/** The dossier collection this thread is bound to, if any. */
	dossier_id: string | null;
	title: string;
	created_by: string;
	state: ConversationState;
	created_at: number;
	updated_at: number;
}

/** One attached document. */
export interface MessageAttachment {
	id: string;
	kind?: 'piece' | 'asset';
	name?: string;
	mime?: string;
}

export interface Message {
	id: string;
	tenant_id: string;
	conversation_id: string;
	role: MessageRole;
	content: string;
	/** Set when this assistant turn is backed by a mission. */
	run_id: string | null;
	attachments?: MessageAttachment[];
	skills?: string[];
	state: MessageState;
	/** The model ran out of completion budget: the text stops mid-answer. */
	truncated?: boolean;
	/** Agent turns only. A missing verification is never read as a pass. */
	agent_outcome?: AgentOutcome | '';
	answer_revision_id?: string | null;
	verification_id?: string | null;
	created_at: number;
	updated_at: number;
}

/** GET /conversations/:id — the whole thread, plus the cursor to resume from. */
export interface ConversationDetail extends Conversation {
	messages: Message[];
	runs: RunSummary[];
	artifacts: Artifact[];
	cursor: number;
	/** The retry key of a creation with a first turn. */
	request_id?: string;
}

export interface ConversationCreateRequest {
	title?: string;
	dossier_id?: string | null;
	/** The composer's normal path: create the thread and its first turn at once. */
	message?: MessageCreateRequest;
	/** The send's retry key; postWithRequestId sets it. */
	request_id?: string;
}

/**
 * PATCH /conversations/:id. `dossier_id` is three-valued on purpose: absent
 * leaves the binding alone, `null` unbinds the collection, a string binds it.
 */
export interface ConversationUpdateRequest {
	title?: string;
	dossier_id?: string | null;
	state?: ConversationState;
}

export interface MessageCreateRequest {
	content: string;
	/** Piece / asset ids attached to the turn. */
	attachment_ids?: string[];
	skill_slugs?: string[];
	/** Binds the thread to a collection on the fly, from the home composer. */
	dossier_id?: string | null;
	/** The send's retry key; postWithRequestId sets it. */
	request_id?: string;
}

/**
 * What the composer gets back immediately. The assistant message exists in
 * `streaming` state before any token does, so the client can open the stream
 * against a row that is already durable.
 */
export interface MessageCreateResponse {
	user_message: Message;
	assistant_message: Message;
	run_id: string | null;
	cursor: number;
	/** Set when the turn entered the agent runtime. */
	agent?: AgentTurnStatus;
	/** The agent turn's retry key: the one sent, or the one the API minted. */
	request_id?: string;
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

/**
 * A turn's attachments as the client renders them. The API persists the ids
 * the composer sent — bare strings — and may return them as such; a thread
 * whose first turn carried documents then crashed on render (every chip keyed
 * on an undefined `id`). Normalise at the boundary so nothing downstream ever
 * sees a string.
 */
export function normalizeMessage(message: Message): Message {
	const raw = (message.attachments ?? []) as unknown[];
	return {
		...message,
		attachments: raw.map((a) =>
			typeof a === 'string' || typeof a === 'number'
				? { id: String(a) }
				: (a as MessageAttachment)
		)
	};
}

function normalizeDetail(detail: ConversationDetail): ConversationDetail {
	return { ...detail, messages: (detail.messages ?? []).map(normalizeMessage) };
}

/**
 * POST /conversations — optionally with the first turn. Resending the same
 * body after an unknown outcome reuses its retry key, so a network retry
 * never opens a second thread.
 */
export async function createConversation(
	body: ConversationCreateRequest = {}
): Promise<ConversationDetail> {
	return normalizeDetail(await postWithRequestId<ConversationDetail>('/conversations', body));
}

/** GET /conversations — the history rail, paginated. */
export async function listConversations(
	query: ListQuery = {}
): Promise<ListResponse<Conversation>> {
	return unwrap(
		await fetchApi<ListResponse<Conversation>>(`/conversations${queryString({ ...query })}`)
	);
}

/** GET /conversations/:id — messages, runs, artifacts and the resume cursor. */
export async function getConversation(id: string): Promise<ConversationDetail> {
	return normalizeDetail(unwrap(await fetchApi<ConversationDetail>(`/conversations/${id}`)));
}

/** PATCH /conversations/:id — rename, archive, bind or unbind the collection. */
export async function updateConversation(
	id: string,
	patch: ConversationUpdateRequest
): Promise<Conversation> {
	return unwrap(await fetchApi<Conversation>(`/conversations/${id}`, 'PATCH', patch));
}

/** DELETE /conversations/:id */
export async function deleteConversation(id: string): Promise<void> {
	unwrap(await fetchApi(`/conversations/${id}`, 'DELETE'));
}

/**
 * POST /conversations/:id/messages — send a turn. It may start a run.
 * Resending the same body after an unknown outcome reuses its retry key.
 */
export async function postMessage(
	id: string,
	body: MessageCreateRequest
): Promise<MessageCreateResponse> {
	const res = await postWithRequestId<MessageCreateResponse>(`/conversations/${id}/messages`, body);
	return {
		...res,
		user_message: normalizeMessage(res.user_message),
		assistant_message: normalizeMessage(res.assistant_message)
	};
}

/**
 * The copy for a refused agent intake (409 active_turn or request_conflict,
 * 422 otherwise), or
 * null when the error is something else. Either way the lawyer's text stays
 * in the composer.
 */
export function intakeErrorKey(err: unknown): string | null {
	const code = (err as Partial<AgentIntakeErrorBody> | null)?.code;
	switch (code) {
		case 'active_turn':
			return 'chat.agent.activeTurn';
		case 'request_conflict':
			return 'chat.agent.requestConflict';
		case 'invalid_competence':
			return 'chat.agent.invalidCompetence';
		default:
			return null;
	}
}

/**
 * GET /conversations/:id/events — one cursor page. Exposed on its own for a
 * catch-up read (e.g. after the tab was hidden) without starting a transport.
 */
export async function getConversationEvents(
	id: string,
	after = 0,
	limit = 100
): Promise<EventsResponse> {
	return unwrap(
		await fetchApi<EventsResponse>(`/conversations/${id}/events${queryString({ after, limit })}`)
	);
}

// ---------------------------------------------------------------------------
// The two transports
// ---------------------------------------------------------------------------

export type Transport = 'stream' | 'poll';

/**
 * Callbacks a transport drives. Everything the UI reacts to arrives through
 * `onEvent`, in cursor order, and the same event id is never emitted twice by
 * one subscription — a replay after a reconnect is deduplicated here as well
 * as in the store.
 */
export interface ConversationHandlers {
	/** One durable event. Unknown types never reach this. */
	onEvent?: (event: RunEvent) => void;
	/** The cursor advanced; persist it if you want to resume later. */
	onCursor?: (after: number) => void;
	/** A transport became live (first connect and every successful reconnect). */
	onOpen?: (transport: Transport) => void;
	/**
	 * The connection dropped and will be retried. This is NOT a failure of the
	 * run and must never be shown as one — at most "reconnexion…".
	 */
	onReconnect?: (info: { transport: Transport; attempt: number; delayMs: number }) => void;
	/** Terminal: the thread is gone, or the session is not allowed to read it. */
	onFatal?: (error: ApiError) => void;
}

/** Why a subscription ended, and where its cursor stood when it did. */
export interface SubscriptionResult {
	cursor: number;
	/**
	 * `aborted`     — the caller's signal fired (navigation, unmount).
	 * `fatal`       — 401 / 403 / 404: retrying cannot help.
	 * `unsupported` — the endpoint or the runtime cannot do SSE; fall back.
	 */
	reason: 'aborted' | 'fatal' | 'unsupported';
	error?: ApiError;
}

interface RetryOptions {
	/** Give up on SSE after this many consecutive failures to connect. */
	maxConnectFailures?: number;
	baseDelayMs?: number;
	maxDelayMs?: number;
}

const DEFAULT_RETRY: Required<RetryOptions> = {
	maxConnectFailures: 4,
	baseDelayMs: 700,
	maxDelayMs: 15_000
};

/** Full jitter on an exponential backoff, so replicas don't reconnect in lockstep. */
function backoffDelay(attempt: number, opts: Required<RetryOptions>): number {
	const ceiling = Math.min(opts.maxDelayMs, opts.baseDelayMs * 2 ** Math.min(attempt, 8));
	return Math.round(ceiling * (0.5 + Math.random() * 0.5));
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
	return new Promise((resolve) => {
		if (signal?.aborted) return resolve();
		const timer = setTimeout(done, ms);
		function done() {
			clearTimeout(timer);
			signal?.removeEventListener('abort', done);
			resolve();
		}
		signal?.addEventListener('abort', done, { once: true });
	});
}

/** 401/403/404 cannot be retried; 501 means "this build has no such endpoint". */
function classifyStatus(status: number): 'fatal' | 'unsupported' | 'retry' {
	if (status === 401 || status === 403 || status === 404) return 'fatal';
	if (status === 501 || status === 405 || status === 415) return 'unsupported';
	return 'retry';
}

async function errorFrom(res: Response): Promise<ApiError> {
	let message = `Request failed (${res.status})`;
	try {
		const body = (await res.json()) as { detail?: string } | null;
		if (body?.detail) message = body.detail;
	} catch {
		/* a non-JSON error body is not worth a second failure */
	}
	return { status: res.status, message };
}

/**
 * Emits an event once, in cursor order. Returns the (possibly unchanged)
 * cursor. Out-of-order or already-seen ids are dropped: the SSE replay after a
 * reconnect overlaps the events we already applied, by design.
 */
function emit(event: unknown, cursor: number, handlers: ConversationHandlers): number {
	if (!event || typeof event !== 'object') return cursor;
	const candidate = event as Partial<RunEvent> & { id?: unknown; type?: unknown };
	const id = Number(candidate.id);
	if (!Number.isFinite(id) || id <= cursor) return cursor;
	if (typeof candidate.type !== 'string' || !isKnownEventType(candidate.type)) {
		// An unknown type still advances the cursor: it is a durable row, and
		// refusing to move past it would stall the thread forever.
		handlers.onCursor?.(id);
		return id;
	}
	handlers.onEvent?.(candidate as RunEvent);
	handlers.onCursor?.(id);
	return id;
}

// --- SSE -------------------------------------------------------------------

interface SSEFrame {
	id?: string;
	event?: string;
	data: string;
}

/** Parses one `\n\n`-delimited SSE frame. Comment lines (`: …`) are heartbeats. */
function parseFrame(raw: string): SSEFrame | null {
	const frame: SSEFrame = { data: '' };
	const dataLines: string[] = [];
	for (const line of raw.split(/\r?\n/)) {
		if (!line || line.startsWith(':')) continue;
		const colon = line.indexOf(':');
		const field = colon === -1 ? line : line.slice(0, colon);
		let value = colon === -1 ? '' : line.slice(colon + 1);
		if (value.startsWith(' ')) value = value.slice(1);
		if (field === 'data') dataLines.push(value);
		else if (field === 'id') frame.id = value;
		else if (field === 'event') frame.event = value;
	}
	if (!dataLines.length && !frame.id) return null;
	frame.data = dataLines.join('\n');
	return frame;
}

/**
 * SSE consumer over the durable cursor. Reconnects for as long as the caller's
 * signal is unaborted, always resuming from the last event id it saw — both as
 * `?after=` and as `Last-Event-ID`, so either server implementation resumes.
 *
 * A dropped connection reaches the caller as `onReconnect`, never as an error:
 * disconnect is loss of observation, not cancellation, and the mission keeps
 * running server-side (§4.5).
 */
export async function streamConversation(
	id: string,
	afterEventId: number,
	handlers: ConversationHandlers = {},
	signal?: AbortSignal,
	options: RetryOptions = {}
): Promise<SubscriptionResult> {
	const opts = { ...DEFAULT_RETRY, ...options };
	let cursor = afterEventId || 0;
	let connectFailures = 0;

	if (typeof fetch !== 'function' || typeof TextDecoder === 'undefined') {
		return { cursor, reason: 'unsupported' };
	}

	while (!signal?.aborted) {
		let response: Response;
		try {
			response = await fetch(
				`${API_BASE}/conversations/${id}/stream${queryString({ after: cursor })}`,
				{
					method: 'GET',
					credentials: 'include',
					headers: {
						Accept: 'text/event-stream',
						// Belt and braces: the cursor travels in the query string
						// AND in the header the SSE spec defines for a resume.
						'Last-Event-ID': String(cursor)
					},
					signal
				}
			);
		} catch {
			// Network-level failure: offline, DNS, TLS, or the abort itself.
			if (signal?.aborted) break;
			connectFailures += 1;
			if (connectFailures >= opts.maxConnectFailures) {
				return { cursor, reason: 'unsupported' };
			}
			const delayMs = backoffDelay(connectFailures, opts);
			handlers.onReconnect?.({ transport: 'stream', attempt: connectFailures, delayMs });
			await sleep(delayMs, signal);
			continue;
		}

		if (!response.ok || !response.body) {
			const kind = response.ok ? 'unsupported' : classifyStatus(response.status);
			if (kind === 'fatal') {
				const error = await errorFrom(response);
				handlers.onFatal?.(error);
				return { cursor, reason: 'fatal', error };
			}
			if (kind === 'unsupported') {
				return { cursor, reason: 'unsupported', error: await errorFrom(response) };
			}
			connectFailures += 1;
			if (connectFailures >= opts.maxConnectFailures) {
				return { cursor, reason: 'unsupported', error: await errorFrom(response) };
			}
			const delayMs = backoffDelay(connectFailures, opts);
			handlers.onReconnect?.({ transport: 'stream', attempt: connectFailures, delayMs });
			await sleep(delayMs, signal);
			continue;
		}

		connectFailures = 0;
		handlers.onOpen?.('stream');

		const reader = response.body.getReader();
		const decoder = new TextDecoder();
		let buffer = '';
		let clean = false;

		try {
			for (;;) {
				const { value, done } = await reader.read();
				if (done) {
					clean = true;
					break;
				}
				buffer += decoder.decode(value, { stream: true });
				let split = buffer.indexOf('\n\n');
				while (split !== -1) {
					const raw = buffer.slice(0, split);
					buffer = buffer.slice(split + 2);
					const frame = parseFrame(raw);
					if (frame && frame.data && frame.event !== 'heartbeat' && frame.event !== 'ping') {
						try {
							const parsed = JSON.parse(frame.data);
							// The event id may live in the frame (`id:`) or in the
							// row itself; the row wins, the frame fills in.
							if (frame.id && parsed && typeof parsed === 'object' && parsed.id == null) {
								parsed.id = Number(frame.id);
							}
							if (frame.event && parsed && typeof parsed === 'object' && parsed.type == null) {
								parsed.type = frame.event;
							}
							cursor = emit(parsed, cursor, handlers);
						} catch {
							/* a malformed frame is dropped; the cursor re-reads it */
						}
					}
					split = buffer.indexOf('\n\n');
				}
			}
		} catch {
			// The connection was severed mid-read. Fall through to the reconnect.
		} finally {
			try {
				await reader.cancel();
			} catch {
				/* already closed */
			}
		}

		if (signal?.aborted) break;

		// The server closed (idle timeout, rollout, pod restart) or the socket
		// broke. Either way: reconnect from the cursor. Never an error.
		const attempt = clean ? 1 : (connectFailures += 1);
		const delayMs = clean ? opts.baseDelayMs : backoffDelay(attempt, opts);
		handlers.onReconnect?.({ transport: 'stream', attempt, delayMs });
		await sleep(delayMs, signal);
	}

	return { cursor, reason: 'aborted' };
}

// --- Polling ---------------------------------------------------------------

export interface PollOptions extends RetryOptions {
	/** Delay after a page that carried events. */
	busyIntervalMs?: number;
	/** Delay after an empty page. */
	idleIntervalMs?: number;
	limit?: number;
}

/**
 * The cursor poll (§4.5): dull, durable, trivially authorizable, and resumable
 * across a pod restart. It feeds exactly the same handlers as the SSE
 * transport, so nothing downstream changes when a client falls back to it.
 */
export async function pollConversation(
	id: string,
	afterEventId: number,
	handlers: ConversationHandlers = {},
	signal?: AbortSignal,
	options: PollOptions = {}
): Promise<SubscriptionResult> {
	const opts = { ...DEFAULT_RETRY, ...options };
	const busy = options.busyIntervalMs ?? 1000;
	const idle = options.idleIntervalMs ?? 2000;
	const limit = options.limit ?? 100;

	let cursor = afterEventId || 0;
	let failures = 0;
	let opened = false;

	while (!signal?.aborted) {
		let response: Response;
		try {
			response = await fetch(
				`${API_BASE}/conversations/${id}/events${queryString({ after: cursor, limit })}`,
				{ method: 'GET', credentials: 'include', headers: { Accept: 'application/json' }, signal }
			);
		} catch {
			if (signal?.aborted) break;
			failures += 1;
			const delayMs = backoffDelay(failures, opts);
			handlers.onReconnect?.({ transport: 'poll', attempt: failures, delayMs });
			await sleep(delayMs, signal);
			continue;
		}

		if (!response.ok) {
			const kind = classifyStatus(response.status);
			const error = await errorFrom(response);
			if (kind === 'fatal') {
				handlers.onFatal?.(error);
				return { cursor, reason: 'fatal', error };
			}
			if (kind === 'unsupported') return { cursor, reason: 'unsupported', error };
			failures += 1;
			const delayMs = backoffDelay(failures, opts);
			handlers.onReconnect?.({ transport: 'poll', attempt: failures, delayMs });
			await sleep(delayMs, signal);
			continue;
		}

		if (!opened) {
			opened = true;
			handlers.onOpen?.('poll');
		} else if (failures > 0) {
			handlers.onOpen?.('poll');
		}
		failures = 0;

		let page: EventsResponse;
		try {
			page = (await response.json()) as EventsResponse;
		} catch {
			await sleep(idle, signal);
			continue;
		}

		const events = page.events ?? [];
		for (const event of events) cursor = emit(event, cursor, handlers);
		if (Number.isFinite(page.next_after) && page.next_after > cursor) {
			cursor = page.next_after;
			handlers.onCursor?.(cursor);
		}

		// A full page means we are behind: read the next one immediately.
		if (events.length >= limit) continue;
		await sleep(events.length ? busy : idle, signal);
	}

	return { cursor, reason: 'aborted' };
}

/**
 * Prefers SSE, falls back to the poll when this build or this browser cannot
 * hold a stream, and keeps the cursor across the switch. Page code should use
 * this and forget the difference exists.
 */
export async function subscribeConversation(
	id: string,
	afterEventId: number,
	handlers: ConversationHandlers = {},
	signal?: AbortSignal
): Promise<SubscriptionResult> {
	const streamed = await streamConversation(id, afterEventId, handlers, signal);
	if (streamed.reason !== 'unsupported' || signal?.aborted) return streamed;
	return pollConversation(id, streamed.cursor, handlers, signal);
}

export type { ApiError, RunEvent, EventsResponse, ListResponse };
