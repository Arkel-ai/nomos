<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		title,
		description = '',
		actions
	}: { title: string; description?: string; actions?: Snippet } = $props();
</script>

<div class="flex flex-wrap items-start justify-between gap-3">
	<div class="space-y-1">
		<h2 class="text-xl font-semibold tracking-tight">{title}</h2>
		{#if description}
			<p class="text-[13px] text-muted-foreground">{description}</p>
		{/if}
	</div>
	{#if actions}
		<div class="flex items-center gap-2">{@render actions()}</div>
	{/if}
</div>
