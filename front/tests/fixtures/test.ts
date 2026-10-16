/**
 * The suite's `test`: every test gets a started MockBackend wired into its
 * page before the first navigation, a fixed theme, and the product tour
 * marked as seen (it would otherwise cover the thread on first paint).
 */
import { readFileSync } from 'node:fs';
import { expect, test as base, type Page } from '@playwright/test';
import { MockBackend } from './backend';
import {
	ANSWER,
	CHRONOLOGY_CALLS,
	activityRows,
	answerDeltas,
	chronologyArtifact,
	chronologyCitations,
	fullCoverage
} from './scenario';

type Theme = 'light' | 'dark';

export const test = base.extend<{ theme: Theme; backend: MockBackend }>({
	theme: ['light', { option: true }],
	backend: async ({ page, theme }, use) => {
		const backend = new MockBackend();
		await backend.start(page);
		await page.addInitScript((mode) => {
			try {
				localStorage.setItem('tour-seen-version', '999');
				localStorage.setItem('mode-watcher-mode', mode);
			} catch {
				/* storage disabled: the page still renders */
			}
		}, theme);
		await use(backend);
		await backend.stop();
		expect(backend.unexpected, 'API calls the suite does not mock').toEqual([]);
		expect(backend.escaped, 'responses that bypassed the mock backend').toEqual([]);
	}
});

export { expect };

// ---------------------------------------------------------------------------
// Copy: assertions read the shipped French locale, never a duplicated string
// ---------------------------------------------------------------------------

const fr = JSON.parse(readFileSync(new URL('../../locales/fr.json', import.meta.url), 'utf8'));

export function L(key: string, vars: Record<string, string | number> = {}): string {
	const value = key.split('.').reduce<unknown>((node, part) => (node as Record<string, unknown> | undefined)?.[part], fr);
	if (typeof value !== 'string') throw new Error(`missing fr locale key ${key}`);
	return value.replace(/\{\{\s*(\w+)\s*\}\}/g, (_, name: string) => String(vars[name] ?? ''));
}

// ---------------------------------------------------------------------------
// The chronology turn, as the agent would play it
// ---------------------------------------------------------------------------

export const THREAD = 'conv-vasseur';
export const RUN = 'run-vasseur-1';
export const REQUEST = 'Établis la chronologie complète du dossier à partir des 18 pièces.';

export interface Turn {
	thread: string;
	run: string;
	assistant: string;
}

/** A thread whose agent turn was just accepted: placeholder answer, run started. */
export function seedRunningTurn(backend: MockBackend, thread = THREAD, run = RUN): Turn {
	backend.addThread(thread);
	const { assistant } = backend.addAgentTurn(thread, { runId: run, userText: REQUEST });
	backend.emit(thread, 'run.created', { workflow_type: 'nomos_agent', workflow_version: 1, message_id: assistant.id }, run);
	backend.emit(thread, 'run.state_changed', { state: 'running' }, run);
	return { thread, run, assistant: assistant.id };
}

/** Both rows of each call (running, then settled), for dispatches `from`..`to`. */
export function playDispatches(backend: MockBackend, turn: Turn, from = 1, to = 8): void {
	for (const call of CHRONOLOGY_CALLS.filter((c) => c.dispatch >= from && c.dispatch <= to)) {
		for (const row of activityRows(call)) backend.activity(turn.thread, turn.run, row);
	}
}

export function playDeltas(backend: MockBackend, turn: Turn, from = 0, to?: number): void {
	for (const delta of answerDeltas().slice(from, to)) {
		backend.emit(turn.thread, 'message.delta', { message_id: turn.assistant, delta }, turn.run);
	}
}

/** Sources and coverage published, the artifact ready, the answer and the run settled. */
export function finishTurn(backend: MockBackend, turn: Turn): void {
	backend.publishSources(turn.run, {
		citations: chronologyCitations(turn.assistant),
		coverage: fullCoverage(turn.run)
	});
	const artifact = chronologyArtifact(turn.run);
	backend.emit(
		turn.thread,
		'artifact.ready',
		{
			artifact_id: artifact.id,
			kind: artifact.kind,
			name: artifact.name,
			char_count: artifact.char_count,
			approved: false,
			document_revision_id: artifact.document_revision_id,
			verification_id: artifact.verification_id,
			partial: false,
			piece_warnings: artifact.piece_warnings
		},
		turn.run
	);
	backend.emit(
		turn.thread,
		'message.complete',
		{ message_id: turn.assistant, content: ANSWER, state: 'complete', agent_outcome: 'completed' },
		turn.run
	);
	backend.emit(turn.thread, 'run.state_changed', { state: 'succeeded' }, turn.run);
}

/** The whole turn already in the durable log: what a lawyer sees reopening it. */
export function seedCompletedTurn(backend: MockBackend, thread = THREAD, run = RUN): Turn {
	const turn = seedRunningTurn(backend, thread, run);
	playDispatches(backend, turn);
	playDeltas(backend, turn);
	finishTurn(backend, turn);
	return turn;
}

/** Opens a thread and waits until its live stream is attached. */
export async function openThread(page: Page, backend: MockBackend, thread = THREAD): Promise<void> {
	await page.goto(`/chat/${thread}`);
	await expect.poll(() => backend.openStreams, { message: 'SSE stream attached' }).toBeGreaterThan(0);
}

/** The assistant turn's answer region (the agent's verified text). */
export function answer(page: Page) {
	return page.locator('.text-read').filter({ hasText: 'Chronologie établie' }).first();
}

// ---------------------------------------------------------------------------
// Cited sources: one compact card, collapsed until opened; each line opens
// the passage sheet, which carries the source's full card
// ---------------------------------------------------------------------------

export function sources(page: Page) {
	return page.getByRole('region', { name: L('citations.title') });
}

/** Opens the sources card; a no-op when it is already open. */
export async function expandSources(page: Page): Promise<void> {
	const toggle = sources(page).getByRole('button', { name: L('citations.title') });
	if ((await toggle.getAttribute('aria-expanded')) !== 'true') await toggle.click();
	await expect(toggle).toHaveAttribute('aria-expanded', 'true');
}

/** One source's line in the expanded card. */
export function sourceLine(page: Page, id: string) {
	return page.locator(`#citation-${id}`);
}

/** Expands the card and opens one source's passage sheet. */
export async function openSource(page: Page, id: string) {
	await expandSources(page);
	await sourceLine(page, id).click();
	const sheet = page.getByRole('dialog');
	await expect(sheet).toBeVisible();
	return sheet;
}

/**
 * A deliverable's card. An agent deliverable shows its title without the
 * extension; the full file name stays in the title attribute.
 */
export function artifactCard(page: Page, name: string) {
	return page.locator('article').filter({ has: page.getByTitle(name, { exact: true }) });
}

/** The coverage card's toggle (its header: title, count and status chip). */
export function coverageToggle(page: Page) {
	return page.getByRole('region', { name: L('coverage.title') }).getByRole('button').first();
}
