import { getMigrations } from 'better-auth/db/migration';
import { auth } from './server';

/**
 * Applies the Better Auth schema (`user`, `session`, `account`,
 * `verification`, plus plugin tables such as `twoFactor`) to the database
 * from the running Node server.
 *
 * Why this exists: in the cluster the front pod just runs the
 * `adapter-node` server — nothing invokes the `@better-auth/cli migrate`
 * step that `dev.sh` runs locally. Without this, the auth tables would
 * never be created in production. Running the migration on boot (via
 * Better Auth's own Kysely migrator — the same one the CLI drives) makes
 * the schema self-applying everywhere, dev and prod alike.
 *
 * Idempotent: when the tables already match the schema, `getMigrations`
 * reports nothing to create/add and this is a no-op. Memoized so it runs
 * once per process; on failure the cache is cleared so the next request
 * retries (covers the brief window when several replicas boot at once).
 */
let pending: Promise<void> | null = null;

export function ensureAuthSchema(): Promise<void> {
	if (!pending) {
		pending = (async () => {
			const { toBeCreated, toBeAdded, runMigrations } = await getMigrations(auth.options);
			if (toBeCreated.length === 0 && toBeAdded.length === 0) return;
			await runMigrations();
		})().catch((err) => {
			pending = null;
			throw err;
		});
	}
	return pending;
}
