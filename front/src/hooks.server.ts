import { building } from '$app/environment';
import { env } from '$env/dynamic/private';
import type { Handle } from '@sveltejs/kit';
import { svelteKitHandler } from 'better-auth/svelte-kit';
import { auth } from '$lib/auth/server';
import { ensureAuthSchema } from '$lib/auth/migrate';
import { ensureBootstrapAdmin } from '$lib/auth/bootstrap';

// Apply the Better Auth schema as soon as the server module loads, so the
// tables exist before the first request (notably in the cluster, where no
// CLI `migrate` step ever runs). Guarded so it never fires during build.
// Then ensure the bootstrap admin account exists (no-op once created).
if (!building && env.DATABASE_URL) {
	ensureAuthSchema()
		.then(ensureBootstrapAdmin)
		.catch((err) => console.error('[auth] boot setup failed', err));
}

/**
 * Routes `/auth/*` to Better Auth; every other request falls through
 * to SvelteKit. All `/api/*` calls are handled by the Go backend
 * (vite proxy in dev, ingress in prod) and never reach here.
 */
export const handle: Handle = async ({ event, resolve }) => {
	// Guarantee the schema is applied before serving an auth request. The
	// promise is memoized, so after the first success this is a no-op await.
	if (!building && env.DATABASE_URL) {
		await ensureAuthSchema();
		await ensureBootstrapAdmin();
	}
	// Private app: tell crawlers not to index any page. Header form of the
	// robots meta tag in app.html, honoured even where the HTML isn't parsed.
	event.setHeaders({ 'X-Robots-Tag': 'noindex, nofollow, noarchive, nosnippet, noimageindex' });
	return svelteKitHandler({ event, resolve, auth, building });
};
