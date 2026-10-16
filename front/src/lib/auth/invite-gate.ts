import { env } from '$env/dynamic/private';
import { pg } from './pg';

/**
 * Invite-only gate. Account creation is allowed only when the email has a
 * valid pending invitation (or is the bootstrap admin). This is enforced at
 * the Better Auth `databaseHooks.user.create.before` layer (see
 * `server.ts`), so it blocks the `/auth/sign-up/email` endpoint itself —
 * not just the UI. A direct API call without an invite is rejected too.
 *
 * The invitations table is owned and migrated by the Go API; we only read
 * it here. Fail closed: callers treat a thrown/false result as "blocked".
 */
const bootstrapEmail = (env.BOOTSTRAP_ADMIN_EMAIL ?? '').trim().toLowerCase();

export async function canCreateAccount(email: string): Promise<boolean> {
	const e = email.trim().toLowerCase();
	if (!e) return false;

	// The bootstrap admin is always allowed (its account is created on boot).
	if (bootstrapEmail && e === bootstrapEmail) return true;

	const now = Math.floor(Date.now() / 1000);
	const res = await pg().query(
		`SELECT 1 FROM invitations
		 WHERE lower(email) = $1 AND status = 'pending' AND expires_at > $2
		 LIMIT 1`,
		[e, now]
	);
	return (res.rowCount ?? 0) > 0;
}
