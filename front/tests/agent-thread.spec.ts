/**
 * The agent thread: activity rows (owner decision 1), live delivery and its
 * recovery paths, the compétence branch (owner decision 5), the flag-gated
 * composers (owner decision 4) and the agent's turn states.
 */
import type { Page } from '@playwright/test';
import type { MockBackend } from './fixtures/backend';
import {
	ANSWER,
	CHRONOLOGY_CALLS,
	SKILLS,
	activityRows,
	answerDeltas,
	chronologyArtifact,
	competenceQuestion,
	fullCoverage
} from './fixtures/scenario';
import {
	L,
	REQUEST,
	RUN,
	THREAD,
	answer,
	expect,
	finishTurn,
	openThread,
	playDeltas,
	playDispatches,
	seedCompletedTurn,
	seedRunningTurn,
	test,
	type Turn
} from './fixtures/test';

const note = (dispatch: number) => CHRONOLOGY_CALLS.find((c) => c.dispatch === dispatch)!.progress_note;
const LAST_SENTENCE = 'la date du message n’a pas été retrouvée dans son texte.';
/** Tool names, argument JSON: never shown to the lawyer. */
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const RAW = /\b(list_pieces|read_piece|search_law|search_caselaw|search_web|verify_output|submit_result|ask_lawyer)\b|"call_id"|\{"/;

const activityRegion = (page: Page) => page.getByRole('region', { name: L('chat.agent.activity') });
/** One row per tool call (the timeline groups them per dispatch). */
const calls = (page: Page) => activityRegion(page).locator('ul > li');
const composer = (page: Page) => page.getByRole('textbox', { name: L('chat.agent.placeholder') });

/** How many times `needle` occurs in the page's rendered text. */
async function occurrences(page: Page, needle: string): Promise<number> {
	const text = await page.locator('body').innerText();
	return text.split(needle).length - 1;
}

/** Reloads and waits until a fresh stream (not the dying one) is attached. */
async function reloadThread(page: Page, backend: MockBackend): Promise<void> {
	const before = backend.streamRequests.length;
	await page.reload();
	await expect.poll(() => backend.streamRequests.length).toBeGreaterThan(before);
	await expect.poll(() => backend.openStreams).toBeGreaterThan(0);
}

// ---------------------------------------------------------------------------
// Activity rows
// ---------------------------------------------------------------------------

test('activity rows: one group per dispatch, live, then collapsible and rebuilt after reload', async ({
	page,
	backend
}) => {
	const turn = seedRunningTurn(backend);
	await openThread(page, backend);
	const activity = activityRegion(page);

	// A call starts: its intent line, its object and a running status.
	const [running, settled] = activityRows(CHRONOLOGY_CALLS[0]);
	backend.activity(turn.thread, turn.run, running);
	await expect(activity.getByText(note(1))).toBeVisible();
	await expect(activity.getByText('Dossier Vasseur · 18 pièces')).toBeVisible();
	await expect(activity.getByText(L('chat.agent.status.running'), { exact: true })).toHaveCount(1);
	await expect(activity.getByText(L('chat.agent.reading')).first()).toBeVisible();

	// The same call_id settles in place: one row, now done, with its counts.
	backend.activity(turn.thread, turn.run, settled);
	await expect(activity.getByText(L('chat.agent.status.ok'), { exact: true })).toHaveCount(1);
	await expect(activity.getByText(L('chat.agent.total', { count: '18' }))).toBeVisible();
	await expect(calls(page)).toHaveCount(1);

	// Dispatch 2 reads two pièces: its note once, both objects, and only the
	// latest dispatch stays on screen while the agent works.
	playDispatches(backend, turn, 2, 2);
	await expect(activity.getByText(note(2))).toHaveCount(1);
	await expect(activity.getByText('Pièce n° 1 · Contrat de travail')).toBeVisible();
	await expect(activity.getByText('Pièce n° 6 · Transcription des SMS')).toBeVisible();
	await expect(activity.getByText(note(1))).toHaveCount(0);

	// A failed search, then the same search succeeding: a modest retry mark.
	playDispatches(backend, turn, 3, 4);
	await expect(activity.getByText(note(4))).toBeVisible();
	await expect(activity.getByText(L('chat.agent.retried'))).toBeVisible();
	await expect(activity.getByText(L('chat.agent.resultsTotal', { count: '2', total: '2' }))).toBeVisible();

	// An empty search never says that no law applies.
	playDispatches(backend, turn, 5, 5);
	await expect(activity.getByText(L('chat.agent.emptySearch'))).toBeVisible();

	playDispatches(backend, turn, 6, 8);
	playDeltas(backend, turn);
	finishTurn(backend, turn);
	await expect(answer(page)).toContainText(LAST_SENTENCE);

	// Delivered: history collapses behind "Voir l’activité".
	const toggle = page.getByRole('button', { name: L('chat.agent.activity') });
	await expect(toggle).toHaveAttribute('aria-expanded', 'false');
	await expect(activity.getByText(note(8))).toHaveCount(0);
	await toggle.click();
	const hide = page.getByRole('button', { name: L('chat.agent.hideActivity') });
	await expect(hide).toHaveAttribute('aria-expanded', 'true');
	for (const call of CHRONOLOGY_CALLS) await expect(activity.getByText(call.progress_note)).toHaveCount(1);
	await expect(calls(page)).toHaveCount(CHRONOLOGY_CALLS.length);
	await expect(activity.getByText(L('chat.agent.status.error'), { exact: true })).toHaveCount(1);
	expect(await activity.innerText()).not.toMatch(RAW);
	// A recovered tool error leaves no failure banner behind.
	await expect(page.getByText(L('chat.agent.failed', { reference: turn.run }))).toHaveCount(0);

	// Reload: rows come back from the durable agent.activity events.
	await reloadThread(page, backend);
	await expect(answer(page)).toContainText(LAST_SENTENCE);
	const again = page.getByRole('button', { name: L('chat.agent.activity') });
	await expect(again).toHaveAttribute('aria-expanded', 'false');
	await again.click();
	for (const call of CHRONOLOGY_CALLS) await expect(activity.getByText(call.progress_note)).toHaveCount(1);
	await expect(calls(page)).toHaveCount(CHRONOLOGY_CALLS.length);
	await expect(activity.getByText(L('chat.agent.retried'))).toBeVisible();
	expect(await activity.innerText()).not.toMatch(RAW);
});

// ---------------------------------------------------------------------------
// Delivery and recovery
// ---------------------------------------------------------------------------

test('reload during delivery resumes from the cursor and lands on the stored answer once', async ({
	page,
	backend
}) => {
	const turn = seedRunningTurn(backend);
	playDispatches(backend, turn);
	await openThread(page, backend);

	playDeltas(backend, turn, 0, 3);
	await expect(answer(page)).toBeVisible();
	const cursorAtReload = backend.cursor;

	await reloadThread(page, backend);
	// Only what was missed is replayed: the stream resumes at the thread cursor.
	expect(backend.streamRequests.at(-1)!.after).toBe(cursorAtReload);
	// The live activity is rebuilt too (latest dispatch on screen).
	await expect(activityRegion(page).getByText(note(8))).toBeVisible();

	playDeltas(backend, turn, 3);
	finishTurn(backend, turn);
	await expect(answer(page)).toContainText(LAST_SENTENCE);
	expect(await occurrences(page, 'Chronologie établie à partir des 18 pièces')).toBe(1);
	expect(await occurrences(page, LAST_SENTENCE)).toBe(1);
	await expect(page.getByRole('button', { name: L('chat.agent.activity') })).toBeVisible();
});

test('a dropped stream falls back to the cursor poll; the work and the answer continue', async ({
	page,
	backend
}) => {
	const turn = seedRunningTurn(backend);
	await openThread(page, backend);
	playDispatches(backend, turn, 1, 2);
	await expect(activityRegion(page).getByText(L('chat.agent.status.ok'), { exact: true })).toHaveCount(2);
	const cursorAtDrop = backend.cursor;

	// The stream is cut and every reconnect is refused.
	backend.stream = 'refuse';
	backend.dropStreams();
	await expect(page.getByText(L('chat.agent.reconnecting'))).toBeVisible();

	// Meanwhile the agent finishes server-side.
	playDispatches(backend, turn, 3, 8);
	playDeltas(backend, turn);
	finishTurn(backend, turn);

	// After the SSE retries give up, the poll takes over from the same cursor.
	await expect(page.getByText(L('chat.pollingFallback'))).toBeVisible({ timeout: 20_000 });
	expect(backend.pollRequests[0].after).toBe(cursorAtDrop);
	await expect(answer(page)).toContainText(LAST_SENTENCE);
	await expect(page.getByText(L('chat.agent.reconnecting'))).toHaveCount(0);
	expect(await occurrences(page, 'Chronologie établie à partir des 18 pièces')).toBe(1);
	await expect(page.getByRole('button', { name: L('artifacts.download') })).toBeEnabled();
});

// ---------------------------------------------------------------------------
// The compétence branch (owner decision 5)
// ---------------------------------------------------------------------------

test.describe('compétence branch', () => {
	test('the / menu stays: it pins a compétence as plain context and sends no tool choice', async ({
		page,
		backend
	}) => {
		seedCompletedTurn(backend);
		await openThread(page, backend);
		const input = composer(page);
		await input.click();
		await page.keyboard.type('/');

		const cph = page.getByRole('option', { name: new RegExp(SKILLS[0].name.replace(/[()]/g, '\\$&')) });
		await expect(cph).toBeVisible();
		await expect(page.getByRole('option', { name: /Mise en demeure/ })).toBeVisible();
		await page.keyboard.type('cph');
		await expect(page.getByRole('option')).toHaveCount(1);
		await page.keyboard.press('Enter');

		// Plain context text with an accessible clear action — not a mission chip.
		await expect(page.getByRole('button', { name: L('chat.composer.clearSkill') })).toBeVisible();
		await expect(page.getByText(SKILLS[0].name, { exact: true })).toBeVisible();
		await expect(page.getByRole('button', { name: L('chat.composer.skills') })).toHaveCount(0);
		await expect(input).toHaveValue('');

		await page.keyboard.type('Rédige la requête devant le conseil de prud’hommes.');
		await page.keyboard.press('Enter');
		await expect.poll(() => backend.sent.length).toBe(1);
		expect(backend.sent[0].body).toEqual({
			content: 'Rédige la requête devant le conseil de prud’hommes.',
			attachment_ids: [],
			skill_slugs: ['requete-cph'],
			request_id: expect.stringMatching(UUID)
		});
	});

	test('a free-text acte: ask_lawyer offers a compétence inline, answered in the thread', async ({
		page,
		backend
	}) => {
		backend.addThread(THREAD, 'Requête prud’homale');
		const { assistant } = backend.addAgentTurn(THREAD, {
			runId: RUN,
			userText: 'Prépare la requête prud’homale de M. Vasseur.'
		});
		backend.emit(THREAD, 'run.created', { workflow_type: 'nomos_agent', workflow_version: 1, message_id: assistant.id }, RUN);
		backend.emit(THREAD, 'run.state_changed', { state: 'running' }, RUN);
		await openThread(page, backend);

		const ask = {
			dispatch: 1,
			call_id: 'call-ask',
			tool: 'ask_lawyer' as const,
			progress_note: 'Je vous demande quelle compétence appliquer à cette requête.',
			object: { kind: 'question' as const, label: 'Choix de la compétence pour la requête' },
			counts: { questions: 1 },
			final: 'ok' as const
		};
		for (const row of activityRows(ask)) backend.activity(THREAD, RUN, row);
		backend.emit(THREAD, 'question.asked', { questions: [competenceQuestion(RUN)] }, RUN);
		backend.emit(THREAD, 'run.state_changed', { state: 'waiting_input' }, RUN);

		// Waiting on the lawyer: the amber state, the question inline.
		await expect(activityRegion(page).getByText(L('chat.agent.waiting')).first()).toBeVisible();
		const card = page.getByRole('region', { name: L('clarification.title') });
		await expect(card.getByText(competenceQuestion(RUN).prompt)).toBeVisible();
		await expect(card.getByText(L('chat.agent.requiredSkip'))).toBeVisible();
		const option = card.getByRole('button', { name: /Requête CPH \(projet\)/ });
		await expect(card.getByRole('button', { name: /Aucune — une note seulement/ })).toBeVisible();

		await option.click();
		await expect(option).toHaveAttribute('aria-pressed', 'true');
		await card.getByRole('button', { name: L('clarification.submit') }).click();
		await expect.poll(() => backend.answers.length).toBe(1);
		expect(backend.answers[0]).toEqual({
			run: RUN,
			answers: [{ question_key: 'competence', values: ['requete-cph'] }]
		});
		// The card settles on what was chosen and the agent resumes.
		await expect(card.getByText('Requête CPH (projet)', { exact: true })).toBeVisible();
		await expect(activityRegion(page).getByText(L('chat.agent.waiting'))).toHaveCount(0);
		// No new turn was posted: the answer continues the same run.
		expect(backend.sent).toHaveLength(0);
	});
});

// ---------------------------------------------------------------------------
// The composer offers no tool control (owner decision 4)
// ---------------------------------------------------------------------------

/** Controls the composer must not offer: choosing tools is the agent's job. */
async function expectNoToolControls(page: Page) {
	await expect(page.getByRole('button', { name: L('chat.composer.skills') })).toHaveCount(0);
	await expect(page.getByRole('switch')).toHaveCount(0);
}

test.describe('the composer', () => {
	test('the thread composer has no web, source, model or mission control', async ({ page, backend }) => {
		seedCompletedTurn(backend);
		await openThread(page, backend);
		await expect(composer(page)).toBeVisible();
		// Attaching stays (it changes the documents, not the tools). Its
		// accessible name is covered — and currently failing — in the a11y spec.
		await expect(page.locator('label:has(input[type="file"])')).toBeVisible();
		await expect(page.getByRole('button', { name: L('chat.composer.send') })).toBeVisible();
		await expectNoToolControls(page);

		await composer(page).fill('Ajoute la pièce n° 9 à la chronologie.');
		await page.keyboard.press('Enter');
		await expect.poll(() => backend.sent.length).toBe(1);
		// The retry key is the only field besides the turn itself.
		expect(Object.keys(backend.sent[0].body).sort()).toEqual(['attachment_ids', 'content', 'request_id', 'skill_slugs']);
		expect(backend.sent[0].body.request_id).toMatch(UUID);
	});

	test('the home composer: no mission chips or quick actions, one intro line', async ({ page, backend }) => {
		await page.goto('/chat');
		await expect(page.getByText(L('chat.agent.intro'))).toBeVisible();
		await expect(composer(page)).toBeVisible();
		await expectNoToolControls(page);

		await composer(page).fill(REQUEST);
		await page.keyboard.press('Enter');
		await expect(page).toHaveURL(/\/chat\/conv-new-\d+$/);
		expect(backend.sent).toHaveLength(1);
		expect(backend.sent[0].body).not.toHaveProperty('sources');
		expect(backend.sent[0].body).not.toHaveProperty('web_enabled');
		await expect(page.getByText(REQUEST)).toBeVisible();
	});
});

// ---------------------------------------------------------------------------
// States
// ---------------------------------------------------------------------------

test.describe('turn states', () => {
	test('working: orb, label and an honest elapsed clock — no spinner, send held with its reason', async ({
		page,
		backend
	}) => {
		const turn = seedRunningTurn(backend);
		await openThread(page, backend);
		playDispatches(backend, turn, 1, 1);
		const activity = activityRegion(page);
		await expect(activity.getByText(/\d+,\d s/)).toBeVisible();
		await expect(activity.locator('.glyph-spin')).toHaveCount(0);
		// Announced politely, without the ticking clock.
		await expect(activity.locator('[aria-live="polite"]')).toHaveText(L('chat.agent.reading'));
		// One substantive turn at a time: send is disabled and says why.
		await composer(page).fill('Autre demande');
		await expect(page.getByRole('button', { name: L('chat.composer.send') })).toBeDisabled();
		await expect(page.getByText(L('chat.agent.activeTurn'))).toBeVisible();
	});

	test('empty instruction: send is disabled with an accessible reason', async ({ page, backend }) => {
		seedCompletedTurn(backend);
		await openThread(page, backend);
		const send = page.getByRole('button', { name: L('chat.composer.send') });
		await expect(send).toBeDisabled();
		await expect(send).toHaveAccessibleDescription(L('chat.agent.emptyInstruction'));
	});

	test('an active-turn conflict keeps the lawyer’s text', async ({ page, backend }) => {
		seedCompletedTurn(backend);
		backend.onSend = () => ({
			status: 409,
			body: { detail: 'active turn', code: 'active_turn', run_id: RUN }
		});
		await openThread(page, backend);
		await composer(page).fill('Ajoute la pièce n° 9.');
		await page.keyboard.press('Enter');
		await expect(page.getByText(L('chat.agent.activeTurn'))).toBeVisible();
		await expect(composer(page)).toHaveValue('Ajoute la pièce n° 9.');
	});

	test('waiting for documents: reading state with the actual pending count', async ({ page, backend }) => {
		const turn = seedRunningTurn(backend);
		backend.message(turn.thread, turn.assistant).state = 'reading';
		const coverage = fullCoverage(turn.run);
		coverage.complete = false;
		coverage.fully_read = 15;
		for (const piece of coverage.pieces.slice(15)) piece.state = 'pending';
		backend.publishSources(turn.run, { coverage });
		await openThread(page, backend);
		await expect(page.getByText(L('chat.agent.readingDocuments'))).toBeVisible();
		await expect(page.getByText(L('chat.agent.pendingDocuments', { count: '3' }))).toBeVisible();
	});

	test('terminal failure: a specific explanation with its reference, no resend', async ({ page, backend }) => {
		const turn = seedRunningTurn(backend);
		await openThread(page, backend);
		playDispatches(backend, turn, 1, 2);
		backend.emit(turn.thread, 'message.failed', { message_id: turn.assistant, agent_outcome: 'agent_failed', content: '' }, turn.run);
		backend.emit(turn.thread, 'run.state_changed', { state: 'failed', failure_code: 'agent_failed' }, turn.run);
		await expect(page.getByText(L('chat.agent.failed', { reference: turn.run }))).toBeVisible();
		await expect(page.getByRole('button', { name: /renvoyer|réessayer/i })).toHaveCount(0);
	});

	test('work limit reached: incomplete-work message, the verified partial document stays', async ({
		page,
		backend
	}) => {
		const turn = seedRunningTurn(backend);
		await openThread(page, backend);
		playDispatches(backend, turn, 1, 4);
		const artifact = { ...chronologyArtifact(turn.run), partial: true };
		backend.emit(
			turn.thread,
			'artifact.ready',
			{ artifact_id: artifact.id, kind: artifact.kind, name: artifact.name, partial: true, piece_warnings: 0 },
			turn.run
		);
		backend.emit(turn.thread, 'message.complete', { message_id: turn.assistant, content: 'Chronologie partielle.', state: 'complete', agent_outcome: 'agent_turn_limit' }, turn.run);
		backend.emit(turn.thread, 'run.state_changed', { state: 'succeeded' }, turn.run);
		await expect(page.getByText(L('chat.agent.limitReached'))).toBeVisible();
		await expect(page.getByText(L('artifacts.partial'))).toBeVisible();
		await expect(page.getByRole('button', { name: L('artifacts.download') })).toBeEnabled();
	});

	test('legal verification refused: explained, and no unchecked document to download', async ({
		page,
		backend
	}) => {
		const turn: Turn = seedRunningTurn(backend);
		await openThread(page, backend);
		playDispatches(backend, turn, 1, 7);
		const artifact = chronologyArtifact(turn.run);
		backend.emit(turn.thread, 'artifact.ready', { artifact_id: artifact.id, kind: artifact.kind, name: artifact.name }, turn.run);
		backend.emit(turn.thread, 'message.failed', { message_id: turn.assistant, agent_outcome: 'verification_refused', content: '' }, turn.run);
		backend.emit(turn.thread, 'run.state_changed', { state: 'failed', failure_code: 'verification_refused' }, turn.run);
		await expect(page.getByText(L('artifacts.verificationBlocked'))).toBeVisible();
		await expect(page.getByRole('button', { name: L('artifacts.download') })).toHaveCount(0);
		await expect(page.getByTitle(artifact.name)).toHaveCount(0);
	});

	test('success: answer and document, activity collapsed, no celebration', async ({ page, backend }) => {
		seedCompletedTurn(backend);
		await openThread(page, backend);
		await expect(answer(page)).toContainText(LAST_SENTENCE);
		await expect(page.getByTitle(chronologyArtifact(RUN).name)).toBeVisible();
		await expect(page.getByRole('button', { name: L('chat.agent.activity') })).toHaveAttribute('aria-expanded', 'false');
		await expect(page.getByText(ANSWER.split('\n')[0])).toHaveCount(1);
		expect(answerDeltas().join('')).toBe(ANSWER);
	});
});
