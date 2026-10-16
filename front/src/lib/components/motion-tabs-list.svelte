<script lang="ts">
	import type { ComponentProps } from 'svelte';
	import * as Tabs from '$lib/components/ui/tabs';
	import { motionQuery } from '$lib/motion';

	let { ref = $bindable(null), children, class: className, ...props }: ComponentProps<typeof Tabs.List> = $props();

	$effect(() => {
		if (!ref) return;
		const list = ref;
		const preference = window.matchMedia(motionQuery);
		const indicator = document.createElement('span');
		indicator.className = 'motion-tab-indicator';
		indicator.setAttribute('aria-hidden', 'true');
		list.append(indicator);
		let previous: { x: number; y: number; width: number; height: number } | undefined;
		let animation: Animation | undefined;
		let frame = 0;

		function update() {
			const active = list.querySelector<HTMLElement>('[data-slot="tabs-trigger"][data-state="active"]');
			if (!active) {
				indicator.hidden = true;
				previous = undefined;
				return;
			}
			const next = { x: active.offsetLeft, y: active.offsetTop, width: active.offsetWidth, height: active.offsetHeight };
			if (!next.width || !next.height) return;
			indicator.hidden = false;
			indicator.style.width = `${next.width}px`;
			indicator.style.height = `${next.height}px`;
			indicator.style.transform = `translate(${next.x}px, ${next.y}px)`;
			if (previous && !preference.matches && Object.keys(next).some((key) => next[key as keyof typeof next] !== previous![key as keyof typeof next])) {
				animation?.cancel();
				animation = indicator.animate([
					{ transform: `translate(${previous.x}px, ${previous.y}px) scale(${previous.width / next.width}, ${previous.height / next.height})` },
					{ transform: `translate(${next.x}px, ${next.y}px) scale(1)` }
				], { duration: 320, easing: getComputedStyle(list).getPropertyValue('--motion-spring').trim() });
			}
			previous = next;
			list.dataset.motionTabs = '';
		}
		function schedule() {
			cancelAnimationFrame(frame);
			frame = requestAnimationFrame(update);
		}
		function stopMotion() {
			if (preference.matches) animation?.cancel();
		}
		const mutation = new MutationObserver(schedule);
		mutation.observe(list, { subtree: true, attributes: true, attributeFilter: ['data-state'], childList: true });
		const resize = new ResizeObserver(schedule);
		resize.observe(list);
		for (const trigger of list.querySelectorAll('[data-slot="tabs-trigger"]')) resize.observe(trigger);
		preference.addEventListener('change', stopMotion);
		update();
		return () => {
			cancelAnimationFrame(frame);
			mutation.disconnect();
			resize.disconnect();
			animation?.cancel();
			indicator.remove();
			delete list.dataset.motionTabs;
			preference.removeEventListener('change', stopMotion);
		};
	});
</script>

<Tabs.List bind:ref class="motion-tabs-list {className ?? ''}" {...props}>
	{@render children?.()}
</Tabs.List>
