<script lang="ts">
	import { tick } from 'svelte';
	import { t, locale } from '$lib/i18n';
	let { id, number, oninspect }: { id: string; number: number; oninspect?: () => void } = $props();
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);
	const formatted = $derived(new Intl.NumberFormat($locale || 'fr').format(number));
	async function reveal() {
		oninspect?.();
		await tick(); // oninspect may have just expanded the source list
		const target = document.getElementById(`citation-${id}`);
		target?.scrollIntoView({ block: 'center', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'instant' : 'smooth' });
		target?.focus({ preventScroll: true });
	}
</script>

<button type="button" class="inline-flex h-6 min-w-6 items-center justify-center rounded-md bg-brand-soft/60 px-1.5 font-mono text-2xs tabular-nums text-brand transition-colors duration-200 hover:bg-brand-soft focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand ring-offset-background" aria-label={tx('citations.reference', { number: formatted })} onclick={reveal}>{formatted}</button>
