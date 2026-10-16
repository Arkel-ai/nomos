<script lang="ts">
	import { untrack } from 'svelte';
	import { locale } from '$lib/i18n';
	import { formatElapsed } from './activity';

	/**
	 * The wait before the first token: a label a light sweeps across, and the
	 * seconds it has taken so far — an honest clock rather than an indefinite
	 * spinner. The orb at work is the turn's avatar beside it, not drawn here.
	 */
	let {
		label,
		startedAt
	}: {
		label: string;
		/** Epoch ms the wait began; defaults to when this row mounted. */
		startedAt?: number;
	} = $props();

	let elapsed = $state(0);
	$effect(() => {
		const from = untrack(() => startedAt) ?? Date.now();
		const id = setInterval(() => (elapsed = Date.now() - from), 100);
		return () => clearInterval(id);
	});

	const clock = $derived(formatElapsed(elapsed / 1000, $locale || 'fr'));
</script>

<div class="flex h-7 items-center gap-2.5" role="status">
	<span class="shimmer text-sm">{label}</span>
	<span class="text-muted-foreground/70 font-mono text-2xs tabular-nums">{clock}</span>
</div>
