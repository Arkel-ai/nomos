import { persisted } from 'svelte-persisted-store';
import { defaultLocale } from '$lib/i18n';

export interface UserData {
	[key: string]: unknown;
}

export interface UserStore {
	authenticated: boolean;
	token: string;
	data: UserData;
	settings?: {
		lang: string;
		[key: string]: unknown;
	};
}

export const user = persisted<UserStore>('user', {
	authenticated: false,
	token: '',
	data: {},
	settings: {
		// Nomos is French-facing, so a first-time visitor must land in French.
		// This is the seed for a brand-new browser only — once someone picks a
		// language it is persisted here and in `nomos.lang`, and that choice wins.
		lang: defaultLocale
	}
});
