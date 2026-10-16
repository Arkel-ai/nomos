<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	// The qualification confidence for one pièce, always visible.
	// PLAN §7.1: low confidence is surfaced, never silently accepted.
	import { t } from '$lib/i18n';
	import { tp } from './i18n-payload';
	import * as Tooltip from '$lib/components/ui/tooltip';

	let {
		confidence,
		threshold = 0.7
	}: { confidence: number | null | undefined; threshold?: number } = $props();

	const known = $derived(typeof confidence === 'number' && confidence > 0);
	const value = $derived(known ? Math.min(1, Math.max(0, confidence as number)) : 0);
	const percent = $derived(Math.round(value * 100));
	const low = $derived(known && value < threshold);
	const labelClass = $derived(
		!known ? 'text-muted-foreground' : low ? 'text-amber' : 'text-muted-foreground'
	);
</script>

<Tooltip.Root>
	<Tooltip.Trigger>
		<div class="flex items-center gap-1.5">
			<div class="bg-muted h-1 w-10 overflow-hidden rounded-full">
				<div
					class="h-full rounded-full {low ? 'bg-amber' : known ? 'bg-pos' : 'bg-muted-foreground/40'}"
					style="width: {known ? percent : 0}%"
				></div>
			</div>
			<span class="font-mono text-2xs tabular-nums {labelClass}">
				{known ? `${percent}%` : '—'}
			</span>
			{#if low}
				<Icon name="danger-triangle" class="h-3 w-3 text-amber" />
			{/if}
		</div>
	</Tooltip.Trigger>
	<Tooltip.Content>
		{#if !known}
			{$t('pieces.confidenceUnknown')}
		{:else if low}
			{$t('pieces.lowConfidence')}
		{:else}
			{$t('pieces.confidence', tp({ percent }))}
		{/if}
	</Tooltip.Content>
</Tooltip.Root>
