import type { TourStep } from '$lib/components/tour.svelte';

// The product tour shown to every user on their first visit to the app shell,
// and replayable from the account menu. It is the app's feature map: when a
// key feature ships (a new nav page, a major capability), add a step here,
// its `tour.app.<key>.{title,body}` strings in both locales, and bump
// APP_TOUR_VERSION so existing users are shown the updated tour once.
//
// Targets are `data-tour` attributes in the root layout. Nav pages are
// addressable by href: `[data-tour="nav:/invoices"]`. The composer carries
// `data-tour="composer"` on the chat pages; elsewhere its step is centered.
export const APP_TOUR_VERSION = 4;

export type AppTourStep = Omit<TourStep, 'title' | 'body'> & {
	/** i18n key under `tour.app`. */
	key: string;
	adminOnly?: boolean;
};

export const appTour: AppTourStep[] = [
	{ key: 'welcome', icon: 'rocket' },
	{ key: 'newThread', target: '[data-tour="nav:/chat"]', side: 'right' },
	{ key: 'agent', icon: 'routing-2' },
	{ key: 'competence', target: '[data-tour="composer"]', side: 'top', icon: 'square-academic-cap' },
	{ key: 'search', target: '[data-tour="search"]', side: 'right' },
	{ key: 'history', target: '[data-tour="history"]', side: 'right' },
	{ key: 'dossiers', target: '[data-tour="nav:/dossiers"]', side: 'right' },
	{ key: 'admin', target: '[data-tour="nav:/admin"]', side: 'right', adminOnly: true },
	{ key: 'account', target: '[data-tour="user-menu"]', side: 'right' },
	{ key: 'done', icon: 'confetti' }
];
