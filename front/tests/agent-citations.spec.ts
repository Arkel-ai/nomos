/**
 * Sources by type: pièce, Légifrance,
 * Judilibre and web citations render distinctly, open their frozen passage,
 * survive errors without restarting the agent, and a pièce warning never
 * blocks the download.
 */
import { readFile } from 'node:fs/promises';
import type { Download, Locator, Page } from '@playwright/test';
import type { AgentCitation } from '../src/lib/api/agent-contracts';
import {
	CITATION_IDS,
	VASSEUR_FILES,
	chronologyArtifact,
	chronologyCitations,
	manyCitations
} from './fixtures/scenario';
import { FILE_BODY } from './fixtures/backend';
import {
	L,
	RUN,
	artifactCard,
	expandSources,
	expect,
	openSource,
	openThread,
	seedCompletedTurn,
	sourceLine,
	sources,
	test
} from './fixtures/test';

const date = (seconds: number) =>
	new Date(seconds * 1000).toLocaleDateString('fr', {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		timeZone: 'Europe/Paris'
	});

/** The browser saved the file the mock served, under the expected name. */
async function expectSaved(file: Download, name: string) {
	expect(await file.failure()).toBeNull();
	expect(file.suggestedFilename()).toBe(name);
	expect(await readFile(await file.path(), 'utf8')).toBe(FILE_BODY);
}

function byType(messageId: string) {
	const all = chronologyCitations(messageId);
	const find = <T extends AgentCitation['source_type']>(type: T) =>
		all.find((c): c is Extract<AgentCitation, { source_type: T }> => c.source_type === type)!;
	return {
		piece: find('piece'),
		legifrance: find('legifrance'),
		judilibre: find('judilibre'),
		web: find('web')
	};
}

/**
 * Opens a source's passage sheet from its line, checks what every type shows
 * there (title, type, the exact quote highlighted), runs the type's own
 * checks, then closes it and expects focus back on the line.
 */
async function inspectSource(page: Page, citation: AgentCitation, own: (sheet: Locator) => Promise<void>) {
	const sheet = await openSource(page, citation.id);
	await expect(sheet.getByRole('heading', { name: citation.label })).toBeVisible();
	await expect(sheet.locator('mark')).toHaveText(citation.quote, { useInnerText: true });
	await expect(sheet).toContainText(L(`citations.types.${citation.source_type}`));
	await own(sheet);
	await sheet.getByRole('button', { name: L('citations.close') }).click();
	await expect(sheet).toBeHidden();
	await expect(sourceLine(page, citation.id)).toBeFocused();
}

test('the four source types render distinctly, each with a working passage view', async ({ page, backend }) => {
	const turn = seedCompletedTurn(backend);
	await openThread(page, backend);
	const c = byType(turn.assistant);

	// Collapsed by default: one card whose header carries the source count.
	const toggle = sources(page).getByRole('button', { name: L('citations.title') });
	await expect(toggle).toHaveAttribute('aria-expanded', 'false');
	await expect(toggle).toContainText(String(chronologyCitations(turn.assistant).length));
	await expandSources(page);

	// One line per source: its number chip, label and locator — a DOCX pièce by
	// paragraph, never a page — and a mismatch dot on the pièce.
	const pieceLine = sourceLine(page, c.piece.id);
	await expect(pieceLine.locator('span').first()).toHaveText(String(c.piece.number));
	await expect(pieceLine).toContainText(c.piece.label);
	await expect(pieceLine).toContainText(L('citations.paragraphMark', { number: '2' }));
	await expect(pieceLine).not.toContainText(L('citations.page', { number: '2' }));
	await expect(pieceLine.getByTitle(L('citations.pieceMismatch'))).toBeVisible();
	await expect(sourceLine(page, c.web.id).getByTitle(L('citations.pieceMismatch'))).toHaveCount(0);

	// Pièce: ordinal, file name, DOCX paragraph (never a page), and its warning.
	await inspectSource(page, c.piece, async (sheet) => {
		await expect(sheet).toContainText(L('citations.pieceOrdinal', { number: '6' }));
		await expect(sheet).toContainText(VASSEUR_FILES[5]);
		await expect(sheet).toContainText(L('citations.paragraphMark', { number: '2' }));
		await expect(sheet).not.toContainText(L('citations.page', { number: '2' }));
		await expect(sheet.getByText(L('citations.pieceMismatch'))).toBeVisible();
	});

	// Légifrance: version id, validity interval, and an honest "unavailable" check.
	await inspectSource(page, c.legifrance, async (sheet) => {
		await expect(sheet).toContainText(L('citations.versionId', { version: 'DEMO-V1' }));
		await expect(sheet).toContainText(
			L('citations.version', { from: date(c.legifrance.valid_from!), to: date(c.legifrance.valid_to!) })
		);
		await expect(sheet).toContainText(L('citations.officialUnavailable'));
		await expect(sheet).toContainText(L('citations.checkPassed.temporal_valid'));
		await expect(sheet).not.toContainText(L('citations.checkPassed.official_matched'));
	});

	// Judilibre: court, chamber, decision date and identity.
	await inspectSource(page, c.judilibre, async (sheet) => {
		for (const text of [c.judilibre.court!, c.judilibre.chamber!, c.judilibre.ecli!, c.judilibre.pourvoi!, date(c.judilibre.decision_date!)]) {
			await expect(sheet).toContainText(text);
		}
		await expect(sheet).toContainText(`${L('citations.checkNames.semantic_support')} · ${L('citations.checkStatus.not_checked')}`);
	});

	// Web: host, URL, retrieval and "excerpt only" — never a legal-authority badge.
	await inspectSource(page, c.web, async (sheet) => {
		await expect(sheet).toContainText(c.web.host!);
		await expect(sheet).toContainText(L('citations.webExcerpt'));
		await expect(sheet).toContainText(L('citations.retrievedAt'));
		const open = sheet.getByRole('link', { name: L('citations.openSourceLink') });
		await expect(open).toHaveAttribute('href', c.web.url);
		await expect(open).toHaveAttribute('target', '_blank');
		await expect(open).toHaveAttribute('rel', /noopener/);
	});
	expect(backend.citationReads).toEqual([c.piece.id, c.legifrance.id, c.judilibre.id, c.web.id]);
});

test('a pièce line locates a PDF by page and paragraph, an XLSX by sheet and cell', async ({ page, backend }) => {
	const turn = seedCompletedTurn(backend);
	const [pdf, xlsx] = manyCitations(turn.assistant, 2);
	pdf.locator = { kind: 'pdf', page: 3, paragraph: 2, start: 0, end: 10 };
	xlsx.locator = { kind: 'xlsx', sheet: 'Chiffrage des demandes', cell: 'D8', start: 0, end: 8 };
	backend.publishSources(turn.run, { citations: [pdf, xlsx] });
	await openThread(page, backend);
	await expandSources(page);
	await expect(sourceLine(page, pdf.id)).toContainText(
		`${L('citations.page', { number: '3' })} · ${L('citations.paragraphMark', { number: '2' })}`
	);
	await expect(sourceLine(page, xlsx.id)).toContainText("'Chiffrage des demandes'!D8");
	await expect(sourceLine(page, xlsx.id)).not.toContainText(L('citations.page', { number: '' }).trim());
});

test('a numbered reference opens the sources and leads to its line', async ({ page, backend }) => {
	seedCompletedTurn(backend);
	await openThread(page, backend);
	await page.getByRole('button', { name: L('citations.reference', { number: '3' }) }).click();
	await expect(sources(page).getByRole('button', { name: L('citations.title') })).toHaveAttribute('aria-expanded', 'true');
	await expect(sourceLine(page, CITATION_IDS.judilibre)).toBeFocused();
});

test('a pièce warning is shown on the source and the document, and download stays enabled', async ({
	page,
	backend
}) => {
	seedCompletedTurn(backend);
	await openThread(page, backend);
	const artifact = chronologyArtifact(RUN);
	const doc = artifactCard(page, artifact.name);
	await expect(doc).toContainText(L('artifacts.pieceWarnings', { count: '2' }));
	await expect(doc).toContainText(L('artifacts.draftBadge'));
	const download = doc.getByRole('button', { name: L('artifacts.download') });
	await expect(download).toBeEnabled();
	const [file] = await Promise.all([page.waitForEvent('download'), download.click()]);
	await expectSaved(file, artifact.name);

	// The original pièce stays downloadable from its source.
	const sheet = await openSource(page, CITATION_IDS.piece);
	await expect(sheet.getByText(L('citations.pieceMismatch'))).toBeVisible();
	const [original] = await Promise.all([
		page.waitForEvent('download'),
		sheet.getByRole('button', { name: L('citations.openSourceLink') }).click()
	]);
	await expectSaved(original, VASSEUR_FILES[5]);
});

test('a failed download link says so inline and retries with a new link', async ({ page, backend }) => {
	seedCompletedTurn(backend);
	backend.downloadFailures.set(chronologyArtifact(RUN).id, 1);
	await openThread(page, backend);
	const doc = artifactCard(page, chronologyArtifact(RUN).name);
	await doc.getByRole('button', { name: L('artifacts.download') }).click();
	await expect(doc.getByText(L('artifacts.downloadError'))).toBeVisible();
	const retry = doc.getByRole('button', { name: L('artifacts.retryDownload') });
	const [file] = await Promise.all([page.waitForEvent('download'), retry.click()]);
	await expectSaved(file, chronologyArtifact(RUN).name);
	await expect(doc.getByText(L('artifacts.downloadError'))).toHaveCount(0);
});

test('a source that fails to load keeps its stored identity; retry refetches the source only', async ({
	page,
	backend
}) => {
	const turn = seedCompletedTurn(backend);
	const legi = byType(turn.assistant).legifrance;
	backend.citationFailures.set(legi.id, 1);
	await openThread(page, backend);
	const sheet = await openSource(page, legi.id);
	await expect(sheet.getByText(L('citations.loadError'))).toBeVisible();
	await expect(sheet.locator('blockquote')).toHaveText(legi.quote);

	await sheet.getByRole('button', { name: L('citations.retryDisplay') }).click();
	await expect(sheet.locator('mark')).toHaveText(legi.quote, { useInnerText: true });
	await expect(sheet.getByText(L('citations.loadError'))).toHaveCount(0);
	expect(backend.citationReads).toEqual([legi.id, legi.id]);
	// Never a restart of the agent.
	expect(backend.sent).toHaveLength(0);
	expect(backend.answers).toHaveLength(0);
});

test('an unavailable source shows its status, never a broken empty sheet', async ({ page, backend }) => {
	const turn = seedCompletedTurn(backend);
	const juri = byType(turn.assistant).judilibre;
	backend.unavailableSources.add(juri.id);
	await openThread(page, backend);
	const sheet = await openSource(page, juri.id);
	await expect(sheet.getByText(L('citations.unavailable'))).toBeVisible();
	await expect(sheet.locator('blockquote')).toHaveText(juri.quote);
	await expect(sheet).toContainText(juri.ecli!);
	await expect(sheet.locator('mark')).toHaveCount(0);
});

test('sources page by 50 in first-use order', async ({ page, backend }) => {
	const turn = seedCompletedTurn(backend);
	backend.publishSources(turn.run, { citations: manyCitations(turn.assistant, 55) });
	await openThread(page, backend);
	await expect(sources(page).getByRole('button', { name: L('citations.title') })).toContainText('55');
	await expandSources(page);
	const list = sources(page).locator('ol > li');
	await expect(list).toHaveCount(50);
	await page.getByRole('button', { name: L('citations.showMore') }).click();
	await expect(list).toHaveCount(55);
	await expect(page.getByRole('button', { name: L('citations.showMore') })).toHaveCount(0);
	const numbers = await list.evaluateAll((lines) =>
		lines.map((line) => Number(line.querySelector('button > span')?.textContent))
	);
	expect(numbers).toEqual(Array.from({ length: 55 }, (_, i) => i + 1));
});
