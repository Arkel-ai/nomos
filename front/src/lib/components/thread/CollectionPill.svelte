<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { t } from '$lib/i18n';

	/**
	 * "Collection du projet · 6" — how a thread binds to a dossier (§11). It is
	 * the same pill on the home and at the top of a thread, so the documents in
	 * scope are never a hidden setting.
	 */

	let {
		count = 0,
		dossierId = null,
		title = '',
		onbind
	}: {
		count?: number;
		dossierId?: string | null;
		title?: string;
		onbind?: () => void;
	} = $props();

	// sveltekit-i18n types the default payload as empty, so an interpolated
	// key (`{{count}}`) needs a widened signature — the same shim the
	// composer uses until the shared config declares one.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);

</script>

{#if dossierId}
	<a
		href="/dossiers/{dossierId}"
		class="bg-muted/70 text-foreground hover:bg-muted inline-flex h-7 max-w-full items-center gap-1.5 rounded-lg px-2.5 text-ui transition-colors"
		title={title}
	>
		<Icon name="folder" active class="text-brand h-3.5 w-3.5 shrink-0" />
		<span class="truncate">{tx('chat.collectionPill', { count })}</span>
	</a>
{:else if !onbind}
	<!-- Nothing to bind from here: a plain statement, not a dead button. -->
	<span class="text-muted-foreground inline-flex h-7 items-center gap-1.5 text-ui">
		<Icon name="folder" class="h-3.5 w-3.5 shrink-0" />
		{$t('chat.collectionEmpty')}
	</span>
{:else}
	<button
		type="button"
		onclick={() => onbind?.()}
		class="text-muted-foreground hover:bg-muted/70 hover:text-foreground inline-flex h-7 cursor-pointer items-center gap-1.5 rounded-lg px-2.5 text-ui transition-colors"
	>
		<Icon name="folder" class="h-3.5 w-3.5 shrink-0" />
		<span>{$t('chat.collectionBind')}</span>
	</button>
{/if}
