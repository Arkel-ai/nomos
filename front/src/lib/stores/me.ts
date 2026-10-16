import { writable } from 'svelte/store';
import { fetchApi } from '$lib/api/fetchApi';

export interface Me {
	id: string;
	email: string;
	name: string;
	email_verified: boolean;
	roles: string[];
	is_admin: boolean;
}

interface MeState {
	loading: boolean;
	loaded: boolean;
	me: Me | null;
}

/**
 * Current user + roles, sourced from GET /api/me. Used to gate admin-only
 * UI (the user panel nav item and the /admin route). This is UX only — the
 * Go API enforces every admin action with Casbin regardless of what the
 * frontend shows.
 */
export const meStore = writable<MeState>({ loading: false, loaded: false, me: null });

let inflight: Promise<Me | null> | null = null;

export function loadMe(force = false): Promise<Me | null> {
	if (inflight && !force) return inflight;
	inflight = (async () => {
		meStore.update((s) => ({ ...s, loading: true }));
		try {
			const { status, data } = await fetchApi<Me>('/me');
			const me = status === 200 ? data : null;
			meStore.set({ loading: false, loaded: true, me });
			return me;
		} catch {
			meStore.set({ loading: false, loaded: true, me: null });
			return null;
		} finally {
			inflight = null;
		}
	})();
	return inflight;
}
