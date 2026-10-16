import { defineConfig, devices } from '@playwright/test';

/**
 * The agent UI suite: deterministic, with every
 * `/api/*` and `/auth/*` call answered by tests/fixtures/backend.ts.
 *
 * The dev server runs on its own fixed port with --strictPort — 5173 is often
 * another project's vite — and never reuses a server already on that port, so
 * the suite cannot silently drive someone else's app. DATABASE_URL is blanked
 * so the Better Auth hook never connects to (or migrates) a real database;
 * the session call is mocked in the browser instead.
 */
const PORT = Number(process.env.NOMOS_E2E_PORT ?? 5297);
const BASE_URL = `http://localhost:${PORT}`;

export default defineConfig({
	testDir: './tests',
	outputDir: './node_modules/.cache/playwright/test-results',
	fullyParallel: true,
	forbidOnly: !!process.env.CI,
	retries: 0,
	workers: process.env.CI ? 2 : 4,
	reporter: [['list']],
	timeout: 45_000,
	expect: { timeout: 7_500 },
	use: {
		baseURL: BASE_URL,
		locale: 'fr-FR',
		timezoneId: 'Europe/Paris',
		trace: 'retain-on-failure'
	},
	projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
	webServer: {
		command: `npx vite dev --port ${PORT} --strictPort`,
		url: BASE_URL,
		reuseExistingServer: false,
		timeout: 120_000,
		stdout: 'ignore',
		stderr: 'pipe',
		env: {
			BETTER_AUTH_URL: BASE_URL,
			DATABASE_URL: ''
		}
	}
});
