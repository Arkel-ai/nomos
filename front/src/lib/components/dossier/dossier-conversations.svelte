<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	// The threads bound to this collection, plus the prominent way in: start a
	// conversation about the dossier. A dossier is what a thread reads from —
	// the thread is the product (PLAN §1, §11).
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { t } from '$lib/i18n';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import EmptyState from '$lib/components/empty-state.svelte';
	import { createConversation, listConversations, type Conversation } from '$lib/api/conversations';

	let { dossierId, dossierTitle }: { dossierId: string; dossierTitle: string } = $props();

	let loading = $state(true);
	let threads = $state<Conversation[]>([]);
	let starting = $state(false);
	let error = $state('');

	onMount(load);

	async function load() {
		loading = true;
		try {
			const page = await listConversations({ limit: 100 });
			threads = (page.items ?? []).filter((c) => c.dossier_id === dossierId);
		} catch (err) {
			error = (err as { message?: string })?.message ?? $t('conversations.loadError');
		} finally {
			loading = false;
		}
	}

	async function start() {
		if (starting) return;
		starting = true;
		error = '';
		try {
			const thread = await createConversation({ dossier_id: dossierId, title: dossierTitle });
			await goto(`/chat/${thread.id}`);
		} catch (err) {
			error = (err as { message?: string })?.message ?? $t('dossiers.startConversationError');
			starting = false;
		}
	}

	function formatDate(seconds: number) {
		return new Date(seconds * 1000).toLocaleDateString();
	}
</script>

<Card.Root class="gap-3 rounded-2xl p-5">
	<div class="flex flex-wrap items-center justify-between gap-3">
		<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
			<Icon name="chat-round-dots" class="text-muted-foreground h-4 w-4" />
			{$t('dossiers.conversations')}
			{#if !loading}
				<span class="text-muted-foreground font-mono text-2xs font-normal tabular-nums">
					{threads.length}
				</span>
			{/if}
		</div>
		<Button
			variant="ghost"
			size="sm"
			class="text-muted-foreground hover:text-foreground -mr-2 h-7 px-2 text-xs"
			disabled={starting}
			onclick={start}
		>
			<Icon name="pen-new-square" class="h-3.5 w-3.5" />
			{$t('nav.newConversation')}
		</Button>
	</div>

	{#if error}
		<div class="text-destructive text-xs">{error}</div>
	{/if}

	{#if loading}
		<div class="space-y-2">
			{#each { length: 2 } as _}
				<Skeleton class="h-11 w-full rounded-xl" />
			{/each}
		</div>
	{:else if !threads.length}
		<EmptyState icon="chat-round-dots" title={$t('dossiers.conversationsNone')} />
	{:else}
		<div class="-mx-2 space-y-0.5">
			{#each threads as thread (thread.id)}
				<button
					type="button"
					onclick={() => goto(`/chat/${thread.id}`)}
					class="group hover:bg-muted/70 flex w-full cursor-pointer items-center gap-3 rounded-xl px-2.5 py-2 text-left transition-colors"
				>
					<span
						class="bg-muted text-muted-foreground group-hover:bg-card flex h-8 w-8 shrink-0 items-center justify-center rounded-lg transition-colors"
					>
						<Icon name="chat-round-line" class="h-4 w-4" />
					</span>
					<div class="min-w-0 flex-1">
						<div class="text-foreground truncate text-ui font-medium">
							{thread.title || $t('conversations.untitled')}
						</div>
						<div class="text-muted-foreground truncate text-2xs">
							{$t('conversations.state.' + thread.state)} · {formatDate(thread.updated_at)}
						</div>
					</div>
					<Icon
						name="arrow-right"
						class="text-muted-foreground h-3.5 w-3.5 shrink-0 -translate-x-1 opacity-0 transition-all group-hover:translate-x-0 group-hover:opacity-100"
					/>
				</button>
			{/each}
		</div>
	{/if}
</Card.Root>
