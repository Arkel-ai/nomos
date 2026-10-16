import { persisted } from 'svelte-persisted-store';

/** The APP_TOUR_VERSION this browser last finished or skipped; 0 = never. */
export const tourSeenVersion = persisted<number>('tour-seen-version', 0);
