<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { onMount } from "svelte";
	import { setMode } from "mode-watcher";
	import { cn } from "$lib/utils";
	import { reducedMotion } from '$lib/motion';

	interface AnimatedThemeTogglerProps {
		class?: string;
		duration?: number;
		title?: string;
	}

	let { class: className, duration = 400, ...props }: AnimatedThemeTogglerProps = $props();

	let isDark = $state(false);
	let buttonRef: HTMLButtonElement | null = $state(null);

	onMount(() => {
		const updateTheme = () => {
			isDark = document.documentElement.classList.contains("dark");
		};

		updateTheme();

		const observer = new MutationObserver(updateTheme);
		observer.observe(document.documentElement, {
			attributes: true,
			attributeFilter: ["class"],
		});

		return () => observer.disconnect();
	});

	const toggleTheme = async () => {
		if (!buttonRef) return;

		// Check if View Transition API is supported
		if (!document.startViewTransition || reducedMotion()) {
			// Fallback for browsers that don't support View Transition API
			const newTheme = !isDark;
			isDark = newTheme;
			setMode(newTheme ? "dark" : "light");
			return;
		}

		await document.startViewTransition(() => {
			const newTheme = !isDark;
			isDark = newTheme;
			setMode(newTheme ? "dark" : "light");
		}).ready;

		const { top, left, width, height } = buttonRef.getBoundingClientRect();
		const x = left + width / 2;
		const y = top + height / 2;
		const maxRadius = Math.hypot(
			Math.max(left, window.innerWidth - left),
			Math.max(top, window.innerHeight - top)
		);

		document.documentElement.animate(
			{
				clipPath: [
					`circle(0px at ${x}px ${y}px)`,
					`circle(${maxRadius}px at ${x}px ${y}px)`,
				],
			},
			{
				duration,
				easing: "ease-in-out",
				pseudoElement: "::view-transition-new(root)",
			}
		);
	};
</script>

<button bind:this={buttonRef} onclick={toggleTheme} class={cn(className)} {...props}>
	{#if isDark}
		<Icon name="sun" class="size-5" />
	{:else}
		<Icon name="moon" class="size-5" />
	{/if}
	<span class="sr-only">Toggle theme</span>
</button>
