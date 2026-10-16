<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { sileo } from '$lib/components/ui/sileo';
	import ConfirmDialog from '$lib/components/confirm-dialog.svelte';
	import { t } from '$lib/i18n';
	import { meStore } from '$lib/stores/me';
	import { listConversations, deleteConversation, type Conversation } from '$lib/api/conversations';
	import { listDossiers, deleteDossier, type Dossier } from '$lib/api/dossiers';
	import type { ListQuery, ListResponse } from '$lib/api/runs';

	/**
	 * The data tab of /settings: erase what this user created. Lists are
	 * firm-wide, so "mine" is filtered on created_by — clearing never touches
	 * a colleague's threads or dossiers. Dossiers go through the real purge
	 * (pièces, originals, extracts, runs, deliverables).
	 */

	// sveltekit-i18n types the default payload as empty, so an interpolated
	// key (`{{count}}`) needs a widened signature.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);

	const me = $derived($meStore.me);

	let loaded = $state(false);
	let threads = $state<Conversation[]>([]);
	let dossiers = $state<Dossier[]>([]);
	let confirmThreads = $state(false);
	let confirmDossiers = $state(false);

	async function everything<T>(list: (q: ListQuery) => Promise<ListResponse<T>>): Promise<T[]> {
		const out: T[] = [];
		for (let offset = 0; ; ) {
			const page = await list({ limit: 100, offset });
			out.push(...page.items);
			offset += page.items.length;
			if (!page.items.length || offset >= page.total) return out;
		}
	}

	async function load() {
		if (!me) return;
		const [allThreads, allDossiers] = await Promise.all([
			everything(listConversations),
			everything(listDossiers)
		]);
		threads = allThreads.filter((c) => c.created_by === me.id);
		dossiers = allDossiers.filter((d) => d.created_by === me.id);
		loaded = true;
	}

	// Waits for /api/me: "mine" needs the user id.
	$effect(() => {
		if (me && !loaded) void load().catch(() => (loaded = true));
	});

	async function clearThreads() {
		let done = 0;
		for (const thread of threads) {
			await deleteConversation(thread.id);
			done++;
		}
		sileo.success({ title: tx('settings.cleared', { count: done }) });
		await load();
	}

	async function clearDossiers() {
		let done = 0;
		let incomplete = 0;
		for (const dossier of dossiers) {
			const purge = await deleteDossier(dossier.id);
			if (purge.complete) done++;
			else incomplete++;
		}
		if (done) sileo.success({ title: tx('settings.cleared', { count: done }) });
		if (incomplete) sileo.warning({ title: $t('dossiers.purgeIncomplete') });
		await load();
	}
</script>

{#snippet row(icon: 'chat-round-dots' | 'folder', label: string, hint: string, count: number, onclear: () => void)}
	<div class="flex flex-wrap items-center gap-4 py-3.5">
		<span class="bg-muted text-muted-foreground flex h-9 w-9 shrink-0 items-center justify-center rounded-xl">
			<Icon name={icon} class="h-[18px] w-[18px]" />
		</span>
		<div class="min-w-0 flex-1">
			<div class="text-foreground text-sm font-medium">{label}</div>
			<div class="text-muted-foreground text-xs">{hint}</div>
		</div>
		<Button
			variant="outline"
			size="sm"
			class="text-destructive hover:bg-destructive/10 hover:text-destructive h-8 px-3 text-ui"
			disabled={!count}
			onclick={onclear}
		>
			<Icon name="trash-bin-trash" class="h-4 w-4" />
			{$t('settings.clearAction')}
		</Button>
	</div>
{/snippet}

<Card.Root class="gap-1 rounded-2xl p-5">
	<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
		<Icon name="database" class="text-muted-foreground h-4 w-4" />
		{$t('settings.dataTitle')}
	</div>
	<p class="text-muted-foreground text-xs">{$t('settings.dataBody')}</p>

	{#if !loaded}
		<div class="space-y-3 pt-3">
			<Skeleton class="h-12 w-full rounded-xl" />
			<Skeleton class="h-12 w-full rounded-xl" />
		</div>
	{:else}
		<div class="divide-border divide-y">
			{@render row(
				'chat-round-dots',
				$t('settings.clearThreads'),
				tx('settings.clearThreadsHint', { count: threads.length }),
				threads.length,
				() => (confirmThreads = true)
			)}
			{@render row(
				'folder',
				$t('settings.clearDossiers'),
				tx('settings.clearDossiersHint', { count: dossiers.length }),
				dossiers.length,
				() => (confirmDossiers = true)
			)}
		</div>
	{/if}
</Card.Root>

<ConfirmDialog
	bind:open={confirmThreads}
	title={$t('settings.clearThreadsTitle')}
	description={$t('settings.clearThreadsBody')}
	details={[tx('settings.clearThreadsHint', { count: threads.length })]}
	confirmLabel={$t('settings.clearAction')}
	onconfirm={clearThreads}
/>

<ConfirmDialog
	bind:open={confirmDossiers}
	title={$t('settings.clearDossiersTitle')}
	description={$t('settings.clearDossiersBody')}
	details={[
		tx('settings.clearDossiersHint', { count: dossiers.length }),
		$t('dossiers.deleteKeepsThreads')
	]}
	confirmLabel={$t('settings.clearAction')}
	onconfirm={clearDossiers}
/>
