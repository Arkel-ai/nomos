import { betterAuth } from 'better-auth';
import { APIError } from 'better-auth/api';
import { twoFactor } from 'better-auth/plugins';
import { Pool } from 'pg';
import { env } from '$env/dynamic/private';
import { canCreateAccount } from './invite-gate';

/**
 * Better Auth server instance. It owns the auth tables (`user`,
 * `session`, `account`, `verification`, `twoFactor`) in the shared Postgres database.
 * The schema is applied automatically on Node-server boot by
 * `ensureAuthSchema()` (see `./migrate.ts`); the
 * `@better-auth/cli` is only needed for manual SQL review.
 *
 * The Go API does NOT call this; it validates requests by reading the
 * `session` table directly (api/middlewares/auth.go). Better Auth stays
 * the single source of truth for sessions.
 */
export const auth = betterAuth({
	// Shown as the issuer in authenticator apps (TOTP two-factor).
	appName: 'Nomos',
	database: new Pool({ connectionString: env.DATABASE_URL }),
	secret: env.BETTER_AUTH_SECRET,
	baseURL: env.BETTER_AUTH_URL,
	// Top-level `/auth` (not `/api/auth`) so it doesn't overlap the Go
	// API's `/api/*` namespace — the prod ingress can route `/api/* → api`
	// and `/auth/*` + `/* → front` with no special-case carve-out.
	basePath: '/auth',
	emailAndPassword: {
		enabled: true
	},
	// Optional TOTP two-factor, turned on per user from /settings. Adds the
	// `user.twoFactorEnabled` column and the `twoFactor` table (secret +
	// hashed backup codes); ensureAuthSchema() applies both on boot. A user
	// with 2FA gets no session from sign-in until /login verifies a code.
	plugins: [twoFactor()],
	// Invite-only gate. Signup is enabled (the accept page calls
	// signUp.email), but account creation is blocked unless the email has a
	// valid pending invitation — or is the bootstrap admin. Enforced at the
	// DB-hook layer so a direct `/auth/sign-up/email` call can't bypass it.
	// Fail closed: any error checking the invite blocks creation.
	databaseHooks: {
		user: {
			create: {
				before: async (user) => {
					const allowed = await canCreateAccount(user.email).catch(() => false);
					if (!allowed) {
						throw new APIError('FORBIDDEN', {
							message: 'You need a valid invitation to create an account.'
						});
					}
					return { data: user };
				}
			}
		}
	}
});
