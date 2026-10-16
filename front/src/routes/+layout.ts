import { redirect } from '@sveltejs/kit';
import { loadTranslations, resolveLocale } from '$lib/i18n';
import { get } from 'svelte/store';
import { user } from '$lib/stores/user';
import { authClient } from '$lib/auth/client';
import type { LayoutLoad } from './$types';

export const ssr = false;
export const prerender = false;

// Routes reachable without a session. Every other route requires one, so a
// new page is protected by default — add a prefix here only for a page that
// must work signed out. Entries match as prefixes, except '/', which only
// matches itself: it is the public landing, and it sends a signed-in visitor
// on to /chat.
const PUBLIC_ROUTES = ['/', '/login', '/accept-invite'];

export const load: LayoutLoad = async ({ url, untrack }) => {
	const usr = get(user);
	// This load runs BEFORE the root layout mounts, so whatever bundle it picks
	// is what the first paint uses. Falling back to a hardcoded 'en' here booted the whole
	// French-facing app in English even though defaultLocale is 'fr'.
	// resolveLocale narrows a stored preference to a locale we ship and falls
	// back to defaultLocale, so there is one source of truth for both.
	const initLocale = resolveLocale(usr.settings?.lang);
	await loadTranslations(initLocale);

	// Auth guard. Reruns on each pathname change, so an expired or revoked
	// session is caught on the next navigation. Asks Better Auth directly (the
	// session source of truth), so it doesn't depend on the Go API being up.
	const path = url.pathname;
	const isPublic = PUBLIC_ROUTES.some((r) => path === r || path.startsWith(r + '/'));
	if (!isPublic) {
		const { data } = await authClient.getSession();
		if (!data) {
			const back = path + untrack(() => url.search);
			redirect(307, `/login?redirect=${encodeURIComponent(back)}`);
		}
	}
	return {};
};
