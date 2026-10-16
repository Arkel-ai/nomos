/**
 * The product tour: the agent step, then the `/` compétence step.
 */
import type { Page } from '@playwright/test';
import { APP_TOUR_VERSION } from '../src/lib/app-tour';
import { L, expect, test } from './fixtures/test';

test.beforeEach(async ({ page, backend }) => {
	void backend; // wires the mock and its init script first; this one runs after it
	await page.addInitScript(() => {
		try {
			localStorage.setItem('tour-seen-version', '0');
		} catch {
			/* storage disabled */
		}
	});
});

/** Walks the tour to its end and returns every step title, in order. */
async function walkTour(page: Page): Promise<string[]> {
	const card = page.locator('[role="dialog"][aria-labelledby="tour-title"]');
	const title = card.locator('#tour-title');
	await expect(card).toBeVisible();
	const titles: string[] = [];
	for (;;) {
		const current = (await title.textContent())?.trim() ?? '';
		titles.push(current);
		const done = card.getByRole('button', { name: L('tour.done'), exact: true });
		if (await done.isVisible()) {
			await done.click();
			break;
		}
		await card.getByRole('button', { name: L('common.next'), exact: true }).click();
		await expect(title).not.toHaveText(current);
	}
	await expect(card).toHaveCount(0);
	return titles;
}

test('the agent step, then the compétence step', async ({ page }) => {
	await page.goto('/chat');
	const titles = await walkTour(page);
	const agent = titles.indexOf(L('tour.app.agent.title'));
	expect(agent).toBeGreaterThan(-1);
	expect(titles[agent + 1]).toBe(L('tour.app.competence.title'));
	expect(await page.evaluate(() => localStorage.getItem('tour-seen-version'))).toBe(String(APP_TOUR_VERSION));
});
