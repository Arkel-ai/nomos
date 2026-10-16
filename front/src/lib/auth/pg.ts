import { Pool } from 'pg';
import { env } from '$env/dynamic/private';

/**
 * Shared Postgres pool for the auth-side reads the front needs that Better
 * Auth itself doesn't expose — currently the invite gate (reading the
 * Go-owned `invitations` table) and the bootstrap-admin check.
 *
 * This mirrors how the Go API reads the Better-Auth-owned tables read-only:
 * here the SvelteKit side reads the app-owned `invitations` table read-only.
 * Server-only (imports `$env/dynamic/private`); never import from a `.svelte`
 * component or `+page.ts`.
 */
let pool: Pool | null = null;

export function pg(): Pool {
	if (!pool) {
		pool = new Pool({ connectionString: env.DATABASE_URL });
	}
	return pool;
}
