// CLI-only Better Auth config.
//
// The runtime app uses src/lib/auth/server.ts, which imports
// `$env/dynamic/private` — a SvelteKit alias the Better Auth CLI can't
// resolve when it loads the config standalone. This file mirrors the
// same options but reads plain process.env so
// `npx @better-auth/cli migrate --config auth.config.ts` works.
//
// Keep the `emailAndPassword` / plugins options here IN SYNC with
// src/lib/auth/server.ts.
//
// NOTE: the invite-only `databaseHooks` gate lives only in server.ts. It is
// runtime behaviour, not schema, so the CLI (which only generates/inspects
// SQL) doesn't need it — and it imports `$env/dynamic/private`, which the
// CLI can't resolve. Do not copy it here.
import { betterAuth } from 'better-auth';
import { twoFactor } from 'better-auth/plugins';
import { Pool } from 'pg';

export const auth = betterAuth({
	database: new Pool({ connectionString: process.env.DATABASE_URL }),
	secret: process.env.BETTER_AUTH_SECRET,
	emailAndPassword: {
		enabled: true
	},
	plugins: [twoFactor()]
});
