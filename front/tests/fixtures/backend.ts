/**
 * A route-mocked Nomos backend for the UI suite.
 *
 * Every `/api/*` and `/auth/*` request the page makes is answered here — none
 * reaches the vite proxy, so the developer API on :8000 and its database are
 * never touched. An unmocked call is answered 404 and recorded; the fixture
 * fails the test if any happened.
 *
 * It keeps the same two things the real API keeps: the durable event log
 * (ids are the cursor) and the rows those events describe, so a thread read
 * after N events looks exactly like the server would show it. The SSE endpoint
 * is a real held-open stream served by a local HTTP server — route.continue()
 * rewrites the request to it — so frames arrive live, one at a time, and a
 * dropped connection is a real dropped socket. The same server plays the
 * presigned download URLs: a browser download is never routed, and a
 * cross-origin attachment is what a real S3 link is anyway.
 */
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import type { Page, Route, Request } from '@playwright/test';
import {
	AGENT_ACTIVITY_EVENT,
	MAX_AGENT_DISPATCHES,
	type AgentActivityPayload,
	type AgentCitation,
	type AgentCitationDetail,
	type CoverageReport
} from '../../src/lib/api/agent-contracts';
import type {
	Conversation,
	ConversationDetail,
	Message,
	MessageCreateRequest,
	MessageCreateResponse
} from '../../src/lib/api/conversations';
import type {
	Artifact,
	EventsResponse,
	QuestionAnswer,
	Run,
	RunDetail,
	RunEvent,
	RunQuestion,
	RunState,
	RunSummary
} from '../../src/lib/api/runs';
import {
	DOSSIER_ID,
	SKILLS,
	TENANT,
	USER_ID,
	citationDetail,
	now,
	vasseurDossier,
	vasseurPieces
} from './scenario';

type EventType = RunEvent['type'];
type EventOf<T extends EventType> = Extract<RunEvent, { type: T }>;

interface RunRows {
	run: Run;
	questions: RunQuestion[];
	artifacts: Artifact[];
	citations: AgentCitation[];
	coverage: CoverageReport | null;
}

interface Thread {
	conversation: Conversation;
	messages: Message[];
	runs: Map<string, RunRows>;
}

interface StreamClient {
	conversation: string;
	res: http.ServerResponse;
}

/** What every mocked download serves. */
export const FILE_BODY = 'fictional e2e file';

export interface Sent {
	conversation: string;
	body: MessageCreateRequest;
}

/** Marks every answer this mock gives, so a response that bypassed it is caught. */
export const MOCK_HEADER = 'x-nomos-e2e-mock';

const json = (route: Route, body: unknown, status = 200) =>
	route.fulfill({
		status,
		contentType: 'application/json',
		headers: { [MOCK_HEADER]: '1' },
		body: JSON.stringify(body)
	});

export class MockBackend {
	/** live: held-open SSE · refuse: connection refused · unsupported: 501. */
	stream: 'live' | 'refuse' | 'unsupported' = 'live';

	readonly sent: Sent[] = [];
	readonly answers: { run: string; answers: QuestionAnswer[] }[] = [];
	readonly createdConversations: unknown[] = [];
	readonly streamRequests: { conversation: string; after: number }[] = [];
	readonly pollRequests: { conversation: string; after: number }[] = [];
	readonly citationReads: string[] = [];
	readonly unexpected: string[] = [];
	/** /api or /auth responses that did not come from this mock (e.g. the vite proxy). */
	readonly escaped: string[] = [];

	/** Citation id → how many reads fail with 500 before one succeeds. */
	readonly citationFailures = new Map<string, number>();
	/** Citation ids whose frozen source is gone. */
	readonly unavailableSources = new Set<string>();
	/** Artifact ids whose download link fails with 500 this many times. */
	readonly downloadFailures = new Map<string, number>();

	/** What a POST /conversations/:id/messages does; the default starts an agent turn. */
	onSend: (thread: string, body: MessageCreateRequest) => MessageCreateResponse | { status: number; body: unknown } =
		(thread, body) => this.startTurn(thread, body);

	private readonly threads = new Map<string, Thread>();
	private readonly events: RunEvent[] = [];
	private readonly clients = new Set<StreamClient>();
	private server: http.Server | null = null;
	private port = 0;
	private sequence = 0;

	// -------------------------------------------------------------------------
	// Lifecycle
	// -------------------------------------------------------------------------

	async start(page: Page): Promise<void> {
		this.server = http.createServer((req, res) => this.serve(req, res));
		await new Promise<void>((resolve) => this.server!.listen(0, '127.0.0.1', resolve));
		this.port = (this.server.address() as AddressInfo).port;

		const context = page.context();
		await context.route(
			(url) => url.pathname.startsWith('/api/'),
			(route, request) => this.handleApi(route, request)
		);
		await context.route(
			(url) => url.pathname.startsWith('/auth/'),
			(route, request) => this.handleAuth(route, request)
		);
		page.on('response', (response) => {
			const url = new URL(response.url());
			const guarded = ['/api/', '/auth/'].some((p) => url.pathname.startsWith(p));
			if (guarded && response.headers()[MOCK_HEADER] !== '1') {
				this.escaped.push(`${response.request().method()} ${url.pathname} ${response.status()}`);
			}
		});
	}

	async stop(): Promise<void> {
		for (const client of this.clients) client.res.destroy();
		this.clients.clear();
		await new Promise<void>((resolve) => (this.server ? this.server.close(() => resolve()) : resolve()));
	}

	// -------------------------------------------------------------------------
	// Seeding
	// -------------------------------------------------------------------------

	/** A thread bound to the Vasseur dossier, optionally with a first agent turn. */
	addThread(id: string, title = 'Chronologie du dossier Vasseur'): Thread {
		const at = now() - 600;
		const thread: Thread = {
			conversation: {
				id,
				tenant_id: TENANT,
				dossier_id: DOSSIER_ID,
				title,
				created_by: USER_ID,
				state: 'active',
				created_at: at,
				updated_at: at
			},
			messages: [],
			runs: new Map()
		};
		this.threads.set(id, thread);
		return thread;
	}

	/** User message, assistant placeholder and its agent run, as intake commits them. */
	addAgentTurn(
		threadId: string,
		opts: { runId: string; userText: string; skill?: string | null; state?: RunState }
	): { user: Message; assistant: Message; run: Run } {
		const thread = this.thread(threadId);
		const at = now() - 30;
		const n = thread.messages.length;
		const user: Message = {
			id: `${threadId}-m${n + 1}`,
			tenant_id: TENANT,
			conversation_id: threadId,
			role: 'user',
			content: opts.userText,
			run_id: null,
			attachments: [],
			skills: opts.skill ? [opts.skill] : [],
			state: 'complete',
			created_at: at,
			updated_at: at
		};
		const assistant: Message = {
			id: `${threadId}-m${n + 2}`,
			tenant_id: TENANT,
			conversation_id: threadId,
			role: 'assistant',
			content: '',
			run_id: opts.runId,
			state: 'streaming',
			agent_outcome: '',
			created_at: at,
			updated_at: at
		};
		const run: Run = {
			id: opts.runId,
			tenant_id: TENANT,
			conversation_id: threadId,
			dossier_id: DOSSIER_ID,
			requested_by: USER_ID,
			workflow_type: 'nomos_agent',
			workflow_version: 1,
			instruction: opts.userText,
			state: opts.state ?? 'pending',
			cancel_requested_at: null,
			failure_code: '',
			created_at: at,
			updated_at: at,
			started_at: at,
			finished_at: null
		};
		thread.messages.push(user, assistant);
		thread.runs.set(run.id, { run, questions: [], artifacts: [], citations: [], coverage: null });
		return { user, assistant, run };
	}

	/** Publishes a run's sources and coverage — read by the client on delivery. */
	publishSources(runId: string, sources: { citations?: AgentCitation[]; coverage?: CoverageReport | null }) {
		const rows = this.runRows(runId);
		if (sources.citations) rows.citations = sources.citations;
		if (sources.coverage !== undefined) rows.coverage = sources.coverage;
	}

	// -------------------------------------------------------------------------
	// The durable log
	// -------------------------------------------------------------------------

	/** Appends one event, applies it to the rows, and pushes it to open streams. */
	emit<T extends EventType>(
		threadId: string,
		type: T,
		payload: EventOf<T>['payload'],
		runId: string | null = null
	): EventOf<T> {
		const event = {
			id: ++this.sequence,
			tenant_id: TENANT,
			run_id: runId,
			conversation_id: threadId,
			run_step_id: null,
			created_at: now(),
			type,
			payload
		} as EventOf<T>;
		this.events.push(event);
		this.applyToRows(event);
		for (const client of this.clients) {
			if (client.conversation === threadId) this.writeFrame(client.res, event);
		}
		return event;
	}

	activity(threadId: string, runId: string, payload: AgentActivityPayload) {
		return this.emit(threadId, AGENT_ACTIVITY_EVENT, payload, runId);
	}

	get cursor(): number {
		return this.sequence;
	}

	/** Open SSE connections right now. */
	get openStreams(): number {
		return this.clients.size;
	}

	/** Severs every open stream mid-read, as a proxy or a pod restart would. */
	dropStreams(): void {
		for (const client of this.clients) client.res.destroy();
		this.clients.clear();
	}

	message(threadId: string, id: string): Message {
		const found = this.thread(threadId).messages.find((m) => m.id === id);
		if (!found) throw new Error(`no message ${id}`);
		return found;
	}

	// -------------------------------------------------------------------------
	// Default intake
	// -------------------------------------------------------------------------

	private startTurn(threadId: string, body: MessageCreateRequest): MessageCreateResponse {
		const runId = `run-${threadId}-${this.thread(threadId).runs.size + 1}`;
		const { user, assistant } = this.addAgentTurn(threadId, {
			runId,
			userText: body.content,
			skill: body.skill_slugs?.[0] ?? null
		});
		this.emit(threadId, 'run.created', { workflow_type: 'nomos_agent', workflow_version: 1, message_id: assistant.id }, runId);
		return {
			user_message: user,
			assistant_message: assistant,
			run_id: runId,
			cursor: this.cursor,
			agent: {
				run_id: runId,
				dispatches: 0,
				max_dispatches: MAX_AGENT_DISPATCHES,
				deadline_at: now() + 1800,
				parked: false
			}
		};
	}

	// -------------------------------------------------------------------------
	// Rows
	// -------------------------------------------------------------------------

	private thread(id: string): Thread {
		const thread = this.threads.get(id);
		if (!thread) throw new Error(`no thread ${id}`);
		return thread;
	}

	private runRows(runId: string): RunRows {
		for (const thread of this.threads.values()) {
			const rows = thread.runs.get(runId);
			if (rows) return rows;
		}
		throw new Error(`no run ${runId}`);
	}

	private applyToRows(event: RunEvent): void {
		const thread = event.conversation_id ? this.threads.get(event.conversation_id) : undefined;
		if (!thread) return;
		const rows = event.run_id ? thread.runs.get(event.run_id) : undefined;
		const touch = (id: string, patch: (m: Message) => void) => {
			const m = thread.messages.find((x) => x.id === id);
			if (m) {
				patch(m);
				m.updated_at = event.created_at;
			}
		};
		switch (event.type) {
			case 'message.delta':
				touch(event.payload.message_id, (m) => (m.content += event.payload.delta));
				break;
			case 'message.complete':
				touch(event.payload.message_id, (m) => {
					m.content = event.payload.content ?? m.content;
					m.state = event.payload.state ?? 'complete';
					m.agent_outcome = event.payload.agent_outcome ?? m.agent_outcome;
				});
				break;
			case 'message.failed':
				touch(event.payload.message_id, (m) => {
					m.content = event.payload.content ?? m.content;
					m.state = 'failed';
					m.agent_outcome = event.payload.agent_outcome ?? m.agent_outcome;
				});
				break;
			case 'run.state_changed':
				if (rows) {
					rows.run.state = event.payload.state;
					rows.run.failure_code = event.payload.failure_code ?? rows.run.failure_code;
				}
				break;
			case 'question.asked':
				if (rows) {
					const keys = new Set(event.payload.questions.map((q) => q.question_key));
					rows.questions = [...rows.questions.filter((q) => !keys.has(q.question_key)), ...event.payload.questions];
				}
				break;
			case 'question.answered':
				if (rows) {
					rows.questions = rows.questions.map((q) =>
						q.question_key === event.payload.question_key
							? { ...q, state: event.payload.skipped ? 'skipped' : 'answered', answered_at: event.created_at }
							: q
					);
				}
				break;
			case 'artifact.ready':
				if (rows && !rows.artifacts.some((a) => a.id === event.payload.artifact_id)) {
					rows.artifacts.push({
						id: event.payload.artifact_id,
						tenant_id: TENANT,
						run_id: rows.run.id,
						dossier_id: DOSSIER_ID,
						kind: event.payload.kind,
						asset_id: `asset-${event.payload.artifact_id}`,
						name: event.payload.name,
						char_count: event.payload.char_count ?? 0,
						template_version: '',
						renderer_version: 'e2e',
						approved: event.payload.approved ?? false,
						approved_by: '',
						approved_at: null,
						document_revision_id: event.payload.document_revision_id,
						verification_id: event.payload.verification_id,
						partial: event.payload.partial,
						piece_warnings: event.payload.piece_warnings,
						created_at: event.created_at
					});
				}
				break;
		}
	}

	private detail(thread: Thread): ConversationDetail {
		const runs: RunSummary[] = [...thread.runs.values()].map(({ run }) => ({
			id: run.id,
			state: run.state,
			workflow_type: run.workflow_type,
			workflow_version: run.workflow_version,
			steps_total: 0,
			steps_done: 0,
			created_at: run.created_at
		}));
		return {
			...thread.conversation,
			messages: structuredClone(thread.messages),
			runs,
			artifacts: [...thread.runs.values()].flatMap((r) => structuredClone(r.artifacts)),
			cursor: this.lastEventOf(thread.conversation.id)
		};
	}

	private runDetail(rows: RunRows): RunDetail {
		return {
			...structuredClone(rows.run),
			steps: [],
			deps: [],
			questions: structuredClone(rows.questions),
			artifacts: structuredClone(rows.artifacts),
			cursor: this.sequence
		};
	}

	private lastEventOf(threadId: string): number {
		return this.events.filter((e) => e.conversation_id === threadId).at(-1)?.id ?? 0;
	}

	private eventsAfter(filter: (e: RunEvent) => boolean, after: number, limit: number): EventsResponse {
		const events = this.events.filter((e) => filter(e) && e.id > after).slice(0, limit);
		return { events, next_after: events.at(-1)?.id ?? after };
	}

	// -------------------------------------------------------------------------
	// SSE
	// -------------------------------------------------------------------------

	/** The held-open SSE stream, and the "presigned" download links. */
	private serve(req: http.IncomingMessage, res: http.ServerResponse): void {
		const url = new URL(req.url ?? '/', 'http://127.0.0.1');
		const file = url.pathname.match(/^\/files\/(.+)$/);
		if (file) {
			// Cross-origin like a presigned S3 URL: the name travels in the header.
			res.writeHead(200, {
				'Content-Type': 'application/octet-stream',
				'Content-Disposition': `attachment; filename*=UTF-8''${file[1]}`
			});
			res.end(FILE_BODY);
			return;
		}
		const match = url.pathname.match(/^\/api\/conversations\/([^/]+)\/stream$/);
		if (!match) {
			res.writeHead(404).end();
			return;
		}
		const conversation = match[1];
		const after = Number(url.searchParams.get('after') ?? req.headers['last-event-id'] ?? 0);
		res.writeHead(200, {
			[MOCK_HEADER]: '1',
			'Content-Type': 'text/event-stream',
			'Cache-Control': 'no-cache, no-transform',
			Connection: 'keep-alive',
			'X-Accel-Buffering': 'no'
		});
		res.flushHeaders();
		// Replay what the client missed, then stay open for live frames.
		for (const event of this.events) {
			if (event.conversation_id === conversation && event.id > after) this.writeFrame(res, event);
		}
		const client: StreamClient = { conversation, res };
		this.clients.add(client);
		req.on('close', () => this.clients.delete(client));
	}

	private fileURL(name: string): string {
		return `http://127.0.0.1:${this.port}/files/${encodeURIComponent(name)}`;
	}

	private writeFrame(res: http.ServerResponse, event: RunEvent): void {
		res.write(`id: ${event.id}\nevent: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
	}

	// -------------------------------------------------------------------------
	// Routes
	// -------------------------------------------------------------------------

	private handleAuth(route: Route, request: Request) {
		const path = new URL(request.url()).pathname;
		if (path === '/auth/get-session') {
			const at = new Date().toISOString();
			return json(route, {
				session: {
					id: 'session-e2e',
					userId: USER_ID,
					token: 'session-token-e2e',
					expiresAt: new Date(Date.now() + 86_400_000).toISOString(),
					createdAt: at,
					updatedAt: at
				},
				user: {
					id: USER_ID,
					email: 'avocate@example.invalid',
					name: 'Camille Exemple',
					emailVerified: true,
					twoFactorEnabled: false,
					createdAt: at,
					updatedAt: at
				}
			});
		}
		this.unexpected.push(`${request.method()} ${path}`);
		return json(route, { detail: 'not mocked' }, 404);
	}

	private async handleApi(route: Route, request: Request) {
		const url = new URL(request.url());
		const path = url.pathname.slice('/api'.length);
		const method = request.method();
		const q = (key: string) => url.searchParams.get(key);
		let m: RegExpMatchArray | null;

		if (method === 'GET' && path === '/health') return json(route, { status: 'ok' });
		if (method === 'GET' && path === '/me') {
			return json(route, {
				id: USER_ID,
				email: 'avocate@example.invalid',
				name: 'Camille Exemple',
				email_verified: true,
				roles: ['user'],
				is_admin: false
			});
		}
		if (method === 'GET' && path === '/skills') {
			return json(route, { items: SKILLS, total: SKILLS.length, limit: 100, offset: 0 });
		}
		if (method === 'GET' && path === '/dossiers') {
			const d = vasseurDossier();
			return json(route, { items: [d], total: 1, limit: 100, offset: 0 });
		}
		if (method === 'GET' && path === `/dossiers/${DOSSIER_ID}`) return json(route, vasseurDossier());
		if (method === 'GET' && path === `/dossiers/${DOSSIER_ID}/pieces`) {
			const pieces = vasseurPieces();
			return json(route, { items: pieces, total: pieces.length, limit: 200, offset: 0 });
		}

		// --- conversations ---------------------------------------------------
		if (method === 'GET' && path === '/conversations') {
			const items = [...this.threads.values()].map((t) => t.conversation);
			return json(route, { items, total: items.length, limit: 20, offset: 0 });
		}
		if (method === 'POST' && path === '/conversations') {
			const body = request.postDataJSON() as { dossier_id?: string | null; message?: MessageCreateRequest };
			this.createdConversations.push(body);
			const id = `conv-new-${this.threads.size + 1}`;
			const thread = this.addThread(id, 'Nouvelle conversation');
			if (body.message) {
				this.sent.push({ conversation: id, body: body.message });
				const result = this.onSend(id, body.message);
				if ('status' in result) return json(route, result.body, result.status);
			}
			return json(route, this.detail(thread), 201);
		}
		if ((m = path.match(/^\/conversations\/([^/]+)$/)) && method === 'GET') {
			const thread = this.threads.get(m[1]);
			return thread ? json(route, this.detail(thread)) : json(route, { detail: 'not found' }, 404);
		}
		if ((m = path.match(/^\/conversations\/([^/]+)\/messages$/)) && method === 'POST') {
			const body = request.postDataJSON() as MessageCreateRequest;
			this.sent.push({ conversation: m[1], body });
			const result = this.onSend(m[1], body);
			if ('status' in result) return json(route, result.body, result.status);
			return json(route, result, 201);
		}
		if ((m = path.match(/^\/conversations\/([^/]+)\/events$/)) && method === 'GET') {
			const conversation = m[1];
			const after = Number(q('after') ?? 0);
			this.pollRequests.push({ conversation, after });
			return json(route, this.eventsAfter((e) => e.conversation_id === conversation, after, Number(q('limit') ?? 100)));
		}
		if ((m = path.match(/^\/conversations\/([^/]+)\/stream$/)) && method === 'GET') {
			const after = Number(q('after') ?? 0);
			this.streamRequests.push({ conversation: m[1], after });
			if (this.stream === 'refuse') return route.abort('connectionrefused');
			if (this.stream === 'unsupported') return json(route, { detail: 'not implemented' }, 501);
			return route.continue({ url: `http://127.0.0.1:${this.port}${url.pathname}${url.search}` });
		}

		// --- runs ------------------------------------------------------------
		if ((m = path.match(/^\/runs\/([^/]+)$/)) && method === 'GET') {
			return json(route, this.runDetail(this.runRows(m[1])));
		}
		if ((m = path.match(/^\/runs\/([^/]+)\/events$/)) && method === 'GET') {
			const runId = m[1];
			return json(route, this.eventsAfter((e) => e.run_id === runId, Number(q('after') ?? 0), Number(q('limit') ?? 100)));
		}
		if ((m = path.match(/^\/runs\/([^/]+)\/answers$/)) && method === 'POST') {
			const runId = m[1];
			const { answers } = request.postDataJSON() as { answers: QuestionAnswer[] };
			this.answers.push({ run: runId, answers });
			const rows = this.runRows(runId);
			const threadId = rows.run.conversation_id!;
			for (const answer of answers) {
				rows.questions = rows.questions.map((qq) =>
					qq.question_key === answer.question_key
						? { ...qq, answer: { values: answer.values, text: answer.text }, answered_by: USER_ID }
						: qq
				);
				this.emit(threadId, 'question.answered', { question_key: answer.question_key, skipped: !!answer.skipped, answered_by: USER_ID }, runId);
			}
			this.emit(threadId, 'run.state_changed', { state: 'running' }, runId);
			return json(route, this.runDetail(rows));
		}
		if ((m = path.match(/^\/runs\/([^/]+)\/citations$/)) && method === 'GET') {
			const all = [...this.runRows(m[1]).citations].sort((a, b) => a.number - b.number);
			const after = Number(q('after') ?? 0);
			const limit = Math.min(Number(q('limit') ?? 50), 100);
			const items = all.filter((c) => c.number > after).slice(0, limit);
			const last = items.at(-1);
			const more = !!last && all.some((c) => c.number > last.number);
			return json(route, { items, total: all.length, next_after: more ? last!.number : null });
		}
		if ((m = path.match(/^\/runs\/([^/]+)\/coverage$/)) && method === 'GET') {
			const coverage = this.runRows(m[1]).coverage;
			return coverage ? json(route, coverage) : json(route, { detail: 'no coverage yet' }, 404);
		}
		if ((m = path.match(/^\/citations\/([^/]+)$/)) && method === 'GET') {
			const id = m[1];
			this.citationReads.push(id);
			const failures = this.citationFailures.get(id) ?? 0;
			if (failures > 0) {
				this.citationFailures.set(id, failures - 1);
				return json(route, { detail: 'source read failed' }, 500);
			}
			const citation = [...this.threads.values()]
				.flatMap((t) => [...t.runs.values()])
				.flatMap((r) => r.citations)
				.find((c) => c.id === id);
			if (!citation) return json(route, { detail: 'not found' }, 404);
			const detail: AgentCitationDetail = citationDetail(citation, !this.unavailableSources.has(id));
			return json(route, detail);
		}

		// --- downloads -------------------------------------------------------
		if ((m = path.match(/^\/artifacts\/([^/]+)\/download$/)) && method === 'GET') {
			const id = m[1];
			const failures = this.downloadFailures.get(id) ?? 0;
			if (failures > 0) {
				this.downloadFailures.set(id, failures - 1);
				return json(route, { detail: 'signing failed' }, 500);
			}
			const artifact = [...this.threads.values()]
				.flatMap((t) => [...t.runs.values()])
				.flatMap((r) => r.artifacts)
				.find((a) => a.id === id);
			if (!artifact) return json(route, { detail: 'not found' }, 404);
			return json(route, { url: this.fileURL(artifact.name), name: artifact.name, expires_in: 60 });
		}
		if ((m = path.match(/^\/pieces\/([^/]+)\/download$/)) && method === 'GET') {
			const piece = vasseurPieces().find((p) => p.id === m![1]);
			if (!piece) return json(route, { detail: 'not found' }, 404);
			return json(route, {
				url: this.fileURL(piece.original_name),
				name: piece.original_name,
				expires_in: 60
			});
		}

		this.unexpected.push(`${method} ${url.pathname}${url.search}`);
		return json(route, { detail: 'not mocked' }, 404);
	}
}
