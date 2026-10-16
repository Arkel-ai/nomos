import { createAuthClient } from 'better-auth/svelte';
import { twoFactorClient } from 'better-auth/client/plugins';

/**
 * Browser-side Better Auth client. Same-origin, so baseURL defaults to
 * `window.location.origin + /auth` — no env config needed. The `/auth`
 * basePath must match the server (`./server.ts`).
 *
 * Usage:
 *   import { authClient } from '$lib/auth/client';
 *   await authClient.signIn.email({ email, password });
 *   const session = authClient.useSession();   // Svelte store
 *
 * `twoFactorClient` adds `authClient.twoFactor.*`. Sign-in for a user with
 * 2FA resolves with `twoFactorRedirect: true` instead of a session; /login
 * handles that by asking for the code, so no redirect hook is set here.
 */
export const authClient = createAuthClient({ basePath: '/auth', plugins: [twoFactorClient()] });
