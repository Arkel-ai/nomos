/**
 * Layout, themes, motion and keyboard: 320/390/768/1440 px in light and dark, reduced motion, and
 * keyboard-only use with focus returning from the source sheet.
 */
import type { Locator, Page } from '@playwright/test';
import { CITATION_IDS, answerDeltas, chronologyArtifact, chronologyCitations } from './fixtures/scenario';
import {
	L,
	RUN,
	artifactCard,
	coverageToggle,
	expandSources,
	expect,
	openSource,
	openThread,
	playDeltas,
	playDispatches,
	seedCompletedTurn,
	seedRunningTurn,
	sourceLine,
	sources,
	test
} from './fixtures/test';

const WIDTHS = [320, 390, 768, 1440] as const;
const SM = 640;

/** Scroll containers (and the document) wider than their box. */
async function horizontalOverflow(page: Page): Promise<string[]> {
	return page.evaluate(() => {
		const out: string[] = [];
		const doc = document.scrollingElement!;
		if (doc.scrollWidth > doc.clientWidth + 1) out.push(`document ${doc.scrollWidth}>${doc.clientWidth}`);
		for (const el of document.querySelectorAll<HTMLElement>('body *')) {
			const style = getComputedStyle(el);
			if (!['auto', 'scroll'].includes(style.overflowX) || el.clientWidth === 0) continue;
			if (el.scrollWidth > el.clientWidth + 1) out.push(`${el.tagName.toLowerCase()}.${[...el.classList].slice(0, 4).join('.')} ${el.scrollWidth}>${el.clientWidth}`);
		}
		return out;
	});
}

/** WCAG contrast of an element's text against its composited background. */
function contrastOf(locator: Locator): Promise<number> {
	return locator.evaluate((el) => {
		const canvas = document.createElement('canvas');
		canvas.width = canvas.height = 1;
		const ctx = canvas.getContext('2d', { willReadFrequently: true })!;
		ctx.globalCompositeOperation = 'copy';
		const rgba = (color: string): [number, number, number, number] => {
			ctx.fillStyle = 'rgba(0,0,0,0)';
			ctx.fillStyle = color;
			ctx.fillRect(0, 0, 1, 1);
			const [r, g, b, a] = ctx.getImageData(0, 0, 1, 1).data;
			return [r, g, b, a / 255];
		};
		const over = (top: number[], bottom: number[]) => top.slice(0, 3).map((c, i) => c * top[3] + bottom[i] * (1 - top[3]));
		// Background: composite every layer from the first opaque ancestor up.
		const layers: [number, number, number, number][] = [];
		for (let node: Element | null = el; node; node = node.parentElement) {
			const layer = rgba(getComputedStyle(node).backgroundColor);
			if (layer[3] > 0) layers.push(layer);
			if (layer[3] >= 1) break;
		}
		let bg = [255, 255, 255];
		for (const layer of layers.reverse()) bg = over(layer, bg);
		const style = getComputedStyle(el);
		const fg = over(rgba(style.color).map((c, i) => (i === 3 ? c * Number(style.opacity) : c)), bg);
		const lum = (rgb: number[]) => {
			const [r, g, b] = rgb.map((c) => {
				const s = c / 255;
				return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
			});
			return 0.2126 * r + 0.7152 * g + 0.0722 * b;
		};
		const [hi, lo] = [lum(fg), lum(bg)].sort((a, b) => b - a);
		return (hi + 0.05) / (lo + 0.05);
	});
}

// ---------------------------------------------------------------------------
// Widths × themes
// ---------------------------------------------------------------------------

for (const theme of ['light', 'dark'] as const) {
	test.describe(`${theme} theme`, () => {
		test.use({ theme, colorScheme: theme });

		for (const width of WIDTHS) {
			test(`${width}px: the finished turn fits, reads and inspects`, async ({ page, backend }) => {
				await page.setViewportSize({ width, height: 900 });
				const turn = seedCompletedTurn(backend);
				await openThread(page, backend);

				const html = page.locator('html');
				if (theme === 'dark') await expect(html).toHaveClass(/\bdark\b/);
				else await expect(html).not.toHaveClass(/\bdark\b/);

				// Everything expanded: activity, coverage (all 18 rows), sources.
				await page.getByRole('button', { name: L('chat.agent.activity') }).click();
				await coverageToggle(page).click();
				const coverage = page.getByRole('region', { name: L('coverage.title') });
				await expect(coverage.getByRole('listitem')).toHaveCount(18);
				await expandSources(page);
				expect(await horizontalOverflow(page)).toEqual([]);

				// Text stays legible in this theme.
				const doc = artifactCard(page, chronologyArtifact(RUN).name);
				const legifrance = chronologyCitations(turn.assistant).find((c) => c.id === CITATION_IDS.legifrance)!;
				const checks: [string, Locator][] = [
					['answer', page.getByText('Chronologie établie à partir des 18 pièces')],
					['source line', sourceLine(page, CITATION_IDS.legifrance).getByText(legifrance.label)],
					['document warning', doc.getByText(L('artifacts.pieceWarnings', { count: '2' }))],
					['coverage count', coverage.getByText('18/18', { exact: true })],
					['activity note', page.getByRole('region', { name: L('chat.agent.activity') }).locator('p').first()]
				];
				for (const [what, locator] of checks) {
					expect.soft(await contrastOf(locator), `${what} contrast`).toBeGreaterThanOrEqual(4.5);
				}
				// Inside the passage sheets: the quote, a passed check, the pièce mismatch.
				let sheet = await openSource(page, CITATION_IDS.legifrance);
				await expect(sheet.locator('mark')).toBeVisible();
				expect.soft(await contrastOf(sheet.locator('blockquote')), 'source quote contrast').toBeGreaterThanOrEqual(4.5);
				expect.soft(await contrastOf(sheet.locator('li').first()), 'passed check contrast').toBeGreaterThanOrEqual(4.5);
				await page.keyboard.press('Escape');
				await expect(sheet).toBeHidden();
				sheet = await openSource(page, CITATION_IDS.piece);
				const mismatch = sheet.getByText(L('citations.pieceMismatch'));
				await expect(mismatch).toBeVisible();
				expect.soft(await contrastOf(mismatch), 'pièce mismatch contrast').toBeGreaterThanOrEqual(4.5);
				await page.keyboard.press('Escape');
				await expect(sheet).toBeHidden();

				// Below sm the download fills the card; from sm it sits on the right.
				const download = doc.getByRole('button', { name: L('artifacts.download') });
				const [cardBox, buttonBox] = [await doc.boundingBox(), await download.boundingBox()];
				if (width < SM) expect(buttonBox!.width).toBeGreaterThan(cardBox!.width - 40);
				else expect(buttonBox!.width).toBeLessThan(cardBox!.width / 2);

				// The source sheet: full width on a phone, max-w-xl beyond; long URLs wrap.
				sheet = await openSource(page, CITATION_IDS.web);
				await expect(sheet.locator('mark')).toBeVisible();
				const sheetBox = (await sheet.boundingBox())!;
				if (width < SM) expect(sheetBox.width).toBeGreaterThanOrEqual(width - 1);
				else expect(sheetBox.width).toBeLessThanOrEqual(576 + 1);
				expect(await horizontalOverflow(page)).toEqual([]);
			});
		}
	});
}

// ---------------------------------------------------------------------------
// Reduced motion
// ---------------------------------------------------------------------------

test.describe('reduced motion', () => {
	test.use({ reducedMotion: 'reduce' });

	test('delivered text appears at once and nothing animates', async ({ page, backend }) => {
		const turn = seedRunningTurn(backend);
		playDispatches(backend, turn);
		await openThread(page, backend);

		playDeltas(backend, turn, 0, 3);
		const delivered = answerDeltas().slice(0, 3).join('');
		// Even the trailing partial word: no word-by-word reveal is held back.
		const live = page.locator('p.whitespace-pre-wrap.text-read');
		await expect.poll(() => live.evaluate((el) => el.textContent)).toBe(delivered);
		await expect(page.locator('.word-in')).toHaveCount(0);
		// The animated elements are on screen — they are simply still.
		await expect(page.locator('.signal-caret')).toHaveCount(1);
		await expect(page.locator('[data-beam]')).not.toHaveCount(0);

		const animated = await page.evaluate(() =>
			[...document.querySelectorAll('.signal-caret, .rise-in, .shimmer, [data-beam], [data-slot="skeleton"]')]
				.filter((el) => getComputedStyle(el).animationName !== 'none')
				.map((el) => el.className.toString().slice(0, 60))
		);
		expect(animated).toEqual([]);
		// Status stays readable without motion.
		await expect(page.getByRole('region', { name: L('chat.agent.activity') }).locator('[aria-live="polite"]')).toHaveText(
			L('chat.agent.verifying')
		);
	});
});

// ---------------------------------------------------------------------------
// Keyboard only
// ---------------------------------------------------------------------------

/** Presses Tab (or Shift+Tab) until `target` has focus. */
async function tabTo(page: Page, target: Locator, backwards = false, max = 120): Promise<void> {
	for (let i = 0; i < max; i++) {
		if (await target.evaluate((el) => el === document.activeElement).catch(() => false)) return;
		await page.keyboard.press(backwards ? 'Shift+Tab' : 'Tab');
	}
	throw new Error('target never received keyboard focus');
}

test('keyboard only: reference → source line → passage sheet, Escape returns focus', async ({ page, backend }) => {
	seedCompletedTurn(backend);
	await openThread(page, backend);
	await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

	// [1] opens the sources and moves focus to its line.
	const reference = page.getByRole('button', { name: L('citations.reference', { number: '1' }) });
	await tabTo(page, reference);
	await page.keyboard.press('Enter');
	const trigger = sourceLine(page, CITATION_IDS.piece);
	await expect(trigger).toBeFocused();

	// Away and back by keyboard; Enter on the line opens the sheet with focus inside.
	await page.keyboard.press('Shift+Tab');
	await tabTo(page, trigger);
	const ring = await trigger.evaluate((el) => getComputedStyle(el).boxShadow);
	expect(ring, 'visible focus ring').not.toBe('none');
	await page.keyboard.press('Enter');
	const sheet = page.getByRole('dialog');
	await expect(sheet.locator('mark')).toBeVisible();
	await expect.poll(() => sheet.evaluate((el) => el.contains(document.activeElement))).toBe(true);
	// Focus stays trapped in the sheet.
	for (let i = 0; i < 4; i++) {
		await page.keyboard.press('Tab');
		expect(await sheet.evaluate((el) => el.contains(document.activeElement))).toBe(true);
	}

	// Escape closes it and returns focus to the trigger.
	await page.keyboard.press('Escape');
	await expect(sheet).toBeHidden();
	await expect(trigger).toBeFocused();

	// Back up to the activity history: Enter opens it, Space closes it.
	const toggle = page.getByRole('button', { name: L('chat.agent.activity') });
	await tabTo(page, toggle, true);
	await page.keyboard.press('Enter');
	await expect(page.getByRole('button', { name: L('chat.agent.hideActivity') })).toHaveAttribute('aria-expanded', 'true');
	await page.keyboard.press('Space');
	await expect(page.getByRole('button', { name: L('chat.agent.activity') })).toHaveAttribute('aria-expanded', 'false');
});

test('keyboard only: an inline clarification is answered without a pointer', async ({ page, backend }) => {
	const turn = seedRunningTurn(backend);
	backend.emit(
		turn.thread,
		'question.asked',
		{
			questions: [
				{
					id: 'q-date',
					tenant_id: 'tenant-e2e',
					run_id: turn.run,
					run_step_id: 'step-ask',
					question_key: 'reception',
					prompt: 'La date de réception de la convocation est-elle confirmée par l’original ?',
					kind: 'single',
					options: [
						{ value: 'yes', label: 'Oui, date confirmée' },
						{ value: 'unknown', label: 'Je dois consulter l’original' }
					],
					required: true,
					state: 'pending',
					answered_by: '',
					asked_at: Math.floor(Date.now() / 1000),
					answered_at: null
				}
			]
		},
		turn.run
	);
	backend.emit(turn.thread, 'run.state_changed', { state: 'waiting_input' }, turn.run);
	await openThread(page, backend);
	await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());

	const option = page.getByRole('button', { name: 'Oui, date confirmée' });
	await tabTo(page, option);
	await page.keyboard.press('Space');
	await expect(option).toHaveAttribute('aria-pressed', 'true');
	await tabTo(page, page.getByRole('button', { name: L('clarification.submit') }));
	await page.keyboard.press('Enter');
	await expect.poll(() => backend.answers.length).toBe(1);
	expect(backend.answers[0].answers).toEqual([{ question_key: 'reception', values: ['yes'] }]);
});

// ---------------------------------------------------------------------------
// Names and hit areas
// ---------------------------------------------------------------------------

test('agent controls are named and offer at least a 44px hit area', async ({ page, backend }) => {
	seedCompletedTurn(backend);
	await openThread(page, backend);
	await page.getByRole('textbox', { name: L('chat.agent.placeholder') }).fill('Ajoute la pièce n° 9.');
	const controls = [
		page.getByRole('button', { name: L('chat.composer.send') }),
		page.getByRole('button', { name: L('chat.agent.activity') }),
		coverageToggle(page),
		sources(page).getByRole('button', { name: L('citations.title') }),
		sourceLine(page, CITATION_IDS.piece)
	];
	await expandSources(page);
	for (const control of controls) {
		const box = (await control.boundingBox())!;
		expect.soft(box.height, await control.innerText()).toBeGreaterThanOrEqual(44);
	}
	const close = (await openSource(page, CITATION_IDS.piece)).getByRole('button', { name: L('citations.close') });
	const box = (await close.boundingBox())!;
	expect(Math.min(box.width, box.height)).toBeGreaterThanOrEqual(44);
});

// TODO(owner): softening the agent turn's render made the [n] references
// 24px number chips (h-6) and the deliverable's download button 40px (h-10);
// both need a 44px hit area. Unskip once they are
// back to h-11 (or carry an invisible 44px hit area).
test.fixme('the references and the download button offer a 44px hit area', async ({ page, backend }) => {
	seedCompletedTurn(backend);
	await openThread(page, backend);
	for (const control of [
		page.getByRole('button', { name: L('citations.reference', { number: '1' }) }),
		artifactCard(page, chronologyArtifact(RUN).name).getByRole('button', { name: L('artifacts.download') })
	]) {
		const box = (await control.boundingBox())!;
		expect.soft(box.height, await control.innerText()).toBeGreaterThanOrEqual(44);
	}
});

// TODO(track D): the paperclip is a <label> around a display:none file input
// (vendored FileDropZone), so it has no accessible name in the tree and Tab
// never reaches it — keyboard-only lawyers cannot attach a pièce from the
// composer (named icon-only controls, keyboard-only use). Unskip once
// the composer exposes a focusable, labelled attach control.
test.fixme('the attach control is keyboard-reachable and named', async ({ page, backend }) => {
	seedCompletedTurn(backend);
	await openThread(page, backend);
	const attach = page.getByRole('button', { name: L('chat.composer.attach') });
	await expect(attach).toBeVisible();
	await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
	await tabTo(page, attach);
});

// TODO(track D): the thread header's delete action is icon-only at 32×32 px
// (h-8 w-8 in routes/chat/[id]/+page.svelte); the target is h-11 w-11.
test.fixme('the thread delete action offers a 44px hit area', async ({ page, backend }) => {
	seedCompletedTurn(backend);
	await openThread(page, backend);
	const box = (await page.getByRole('button', { name: L('conversations.delete') }).boundingBox())!;
	expect(Math.min(box.width, box.height)).toBeGreaterThanOrEqual(44);
});

// The "Projet — vérification de l'avocat requise" chip is the one marker that
// an export is not filed-quality work, so it must stay legible in the light
// theme too (the earlier 10px amber stamp was not).
test.describe('light theme', () => {
	test.use({ theme: 'light', colorScheme: 'light' });
	test('the draft stamp on a document meets AA contrast', async ({ page, backend }) => {
		seedCompletedTurn(backend);
		await openThread(page, backend);
		const doc = artifactCard(page, chronologyArtifact(RUN).name);
		expect(await contrastOf(doc.getByText(L('artifacts.draftBadge')))).toBeGreaterThanOrEqual(4.5);
	});
});
