import { env } from '$env/dynamic/private';
import { auth } from './server';
import { pg } from './pg';

/**
 * Bootstrap the first admin on Node-server boot. Because Better Auth owns
 * the `user` table (and password hashing), the first admin *account* must
 * be created here, on the front; the Go API then grants it the "admin"
 * Casbin role (on boot, or lazily on the admin's first request — see
 * api/services/invitations.go).
 *
 * BOOTSTRAP_ADMIN_EMAIL and BOOTSTRAP_ADMIN_PASSWORD have no default here:
 * seeding is skipped when either is unset. front/.env.template ships
 * admin@example.com / NOMOS2026 for local use; a deployment injects its own.
 *
 * Idempotent and memoized: a no-op once the account exists.
 */
const email = (env.BOOTSTRAP_ADMIN_EMAIL ?? '').trim().toLowerCase();
const password = env.BOOTSTRAP_ADMIN_PASSWORD ?? '';

let pending: Promise<void> | null = null;

export function ensureBootstrapAdmin(): Promise<void> {
	if (!pending) {
		pending = (async () => {
			if (!email || !password) {
				console.warn('[bootstrap] BOOTSTRAP_ADMIN_EMAIL or BOOTSTRAP_ADMIN_PASSWORD unset — no admin seeded');
				return;
			}

			const { rowCount } = await pg().query(
				'SELECT 1 FROM "user" WHERE lower(email) = $1 LIMIT 1',
				[email]
			);
			if ((rowCount ?? 0) > 0) return;

			await auth.api.signUpEmail({ body: { name: 'Admin', email, password } });
			console.warn(`[bootstrap] created bootstrap admin "${email}"`);
		})().catch((err) => {
			pending = null;
			console.error('[bootstrap] ensureBootstrapAdmin failed', err);
		});
	}
	return pending;
}
