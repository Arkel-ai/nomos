<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import type { SolarIconName } from '$lib/icons/solar';
	import type { Snippet } from 'svelte';

	let {
		icon,
		title,
		hint = '',
		children
	}: { icon: SolarIconName; title: string; hint?: string; children?: Snippet } = $props();
</script>

<div class="flex flex-col items-center gap-2 py-12 text-center">
	<div
		class="flex h-10 w-10 items-center justify-center rounded-xl border border-dashed border-border/80 bg-muted/40 text-muted-foreground"
	>
		<Icon name={icon} class="h-[18px] w-[18px]" />
	</div>
	<div class="text-[12.5px] font-medium text-foreground">{title}</div>
	{#if hint}
		<div class="text-[11.5px] text-muted-foreground">{hint}</div>
	{/if}
	{#if children}
		<div class="pt-1">{@render children()}</div>
	{/if}
</div>
