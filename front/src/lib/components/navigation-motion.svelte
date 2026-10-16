<script lang="ts">
	import { onNavigate } from '$app/navigation';
	import { reducedMotion } from '$lib/motion';
	let sequence = 0;

	// Animate only page content: the sidebar remains live and keeps its position.
	// No keyed route wrapper, so query-string changes preserve forms and local state.
	onNavigate((navigation) => {
		if (
			reducedMotion() ||
			!document.startViewTransition ||
			navigation.from?.url.pathname === navigation.to?.url.pathname
		) return;

		return new Promise<void>((resolve) => {
			const token = String(++sequence);
			document.documentElement.dataset.routeTransition = token;
			const transition = document.startViewTransition(async () => {
				resolve();
				await navigation.complete;
			});
			// A skipped/interrupted snapshot must never block navigation.
			void transition.ready.catch(() => resolve());
			const clearTransition = () => {
				if (document.documentElement.dataset.routeTransition === token) {
					delete document.documentElement.dataset.routeTransition;
				}
			};
			void transition.finished.then(clearTransition, clearTransition);
		});
	});
</script>
