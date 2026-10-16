<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	import { t } from '$lib/i18n';
	import type { Transport } from '$lib/api/conversations';

	/**
	 * A dropped connection is loss of OBSERVATION, never a failed or cancelled
	 * run (docs/PLAN.md §4.5). So this is quiet and never an error state: it
	 * says "reconnexion…", and it says when the view fell back to the cursor
	 * poll. Cancelling a run is a separate, explicit button in the plan monitor.
	 */

	let {
		reconnecting = false,
		transport = null
	}: { reconnecting?: boolean; transport?: Transport | null } = $props();
</script>

{#if reconnecting}
	<span
		class="text-muted-foreground bg-muted/70 inline-flex h-7 items-center gap-1.5 rounded-lg px-2.5 text-xs"
	>
		<Spinner class="h-3 w-3" />
		{$t('chat.reconnecting')}
	</span>
{:else if transport === 'poll'}
	<span class="text-muted-foreground/70 inline-flex h-7 items-center gap-1.5 text-xs">
		<Icon name="radio" class="h-3 w-3" />
		{$t('chat.pollingFallback')}
	</span>
{/if}
