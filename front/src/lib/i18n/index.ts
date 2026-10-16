import i18n from 'sveltekit-i18n';

/**
 * Nomos speaks French first (docs/PLAN.md §11): `locales/fr.json` is the
 * reference file and `locales/en.json` follows it. The fallback is `fr` too —
 * a key that only exists in the reference must render as French, never as a
 * raw key path.
 */
export const defaultLocale = 'fr';

/** The locales the app ships, in menu order. */
export const supportedLocales = ['fr', 'en'] as const;
export type SupportedLocale = (typeof supportedLocales)[number];

/** Narrows an arbitrary string (a stored preference, a query param) to a
 *  locale we actually ship; anything else falls back to French. */
export function resolveLocale(value: string | null | undefined): SupportedLocale {
	return (supportedLocales as readonly string[]).includes(value ?? '')
		? (value as SupportedLocale)
		: defaultLocale;
}

const config = {
	fallbackLocale: defaultLocale,
	loaders: [
		{
			locale: 'fr',
			key: '',
			loader: async () => (await import('../../../locales/fr.json')).default
		},
		{
			locale: 'en',
			key: '',
			loader: async () => (await import('../../../locales/en.json')).default
		}
	]
};

export const {
	t,
	loading,
	locales,
	locale,
	translations,
	loadTranslations,
	addTranslations,
	setLocale,
	setRoute
} = new i18n(config);
