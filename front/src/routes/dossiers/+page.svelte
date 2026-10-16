<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	// =====================================================================
	// /dossiers — the collections a conversation binds to (PLAN §11).
	// Secondary surface: the thread is the product, this is what it reads.
	// The list-page layout — same Card rhythm, same header
	// layout, same table styling, same empty state and pagination footer.
	//
	// The list endpoint paginates but takes no search or filter parameter,
	// so the page loads the tenant's dossiers once (the store caps at 100)
	// and filters + paginates in the browser; the count shown is the API's.
	// =====================================================================
	import { onMount } from 'svelte';
	import { reveal } from '$lib/motion';
	import { goto } from '$app/navigation';
	import { t } from '$lib/i18n';
	import { tp } from '$lib/components/dossier/i18n-payload';
	import * as Card from '$lib/components/ui/card';
	import * as Table from '$lib/components/ui/table';
	import * as Select from '$lib/components/ui/select';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { Input } from '$lib/components/ui/input';
	import { Button } from '$lib/components/ui/button';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { sileo } from '$lib/components/ui/sileo';
	import EmptyState from '$lib/components/empty-state.svelte';
	import DossierCreateSheet from '$lib/components/dossier/dossier-create-sheet.svelte';
	import DossierStateChip from '$lib/components/dossier/dossier-state-chip.svelte';
	import ConfirmDialog from '$lib/components/confirm-dialog.svelte';
	import { dossiers, loadDossiers } from '$lib/stores/dossier';
	import { deleteDossier, matterTypes, type Dossier, type DossierState } from '$lib/api/dossiers';

	const STATES: DossierState[] = ['active', 'archived', 'purge_pending'];
	let query = $state('');
	let matterFilter = $state('all');
	let stateFilter = $state<'all' | DossierState>('all');
	let pageIndex = $state(0);
	let createOpen = $state(false);
	let deleting = $state<string | null>(null);
	const pageSize = 10;

	onMount(() => {
		loadDossiers();
	});

	const loaded = $derived($dossiers.loaded);
	const total = $derived($dossiers.total);

	const filtered = $derived.by(() => {
		const needle = query.trim().toLowerCase();
		return $dossiers.items.filter((d) => {
			if (matterFilter !== 'all' && d.matter_type !== matterFilter) return false;
			if (stateFilter !== 'all' && d.state !== stateFilter) return false;
			if (!needle) return true;
			return (
				d.title.toLowerCase().includes(needle) ||
				(d.client_reference ?? '').toLowerCase().includes(needle)
			);
		});
	});

	const totalPages = $derived(Math.max(1, Math.ceil(filtered.length / pageSize)));
	const rows = $derived(filtered.slice(pageIndex * pageSize, (pageIndex + 1) * pageSize));
	const pageInfo = $derived.by(() => {
		const start = filtered.length === 0 ? 0 : pageIndex * pageSize + 1;
		return { start, end: Math.min(filtered.length, (pageIndex + 1) * pageSize) };
	});
	const filterCount = $derived(
		(query.trim() ? 1 : 0) + (matterFilter !== 'all' ? 1 : 0) + (stateFilter !== 'all' ? 1 : 0)
	);

	$effect(() => {
		filtered.length;
		if (pageIndex > totalPages - 1) pageIndex = totalPages - 1;
	});

	function resetFilters() {
		query = '';
		matterFilter = 'all';
		stateFilter = 'all';
		pageIndex = 0;
	}

	function matterLabel(value: string) {
		return (matterTypes as readonly string[]).includes(value)
			? $t(`dossiers.matterTypes.${value}`)
			: value;
	}

	function formatDate(seconds: number) {
		return new Date(seconds * 1000).toLocaleDateString();
	}

	async function created(dossier: Dossier) {
		await loadDossiers(true);
		await goto(`/dossiers/${dossier.id}`);
	}

	// Deleting purges everything the dossier holds; the dialog says so first.
	let toDelete = $state<Dossier | null>(null);
	let deleteOpen = $state(false);

	function remove(dossier: Dossier) {
		toDelete = dossier;
		deleteOpen = true;
	}

	async function confirmRemove() {
		const dossier = toDelete;
		if (!dossier) return;
		deleting = dossier.id;
		try {
			const purge = await deleteDossier(dossier.id);
			if (purge.complete) sileo.success({ title: $t('dossiers.deleted') });
			else sileo.warning({ title: $t('dossiers.purgeIncomplete') });
			await loadDossiers(true);
		} finally {
			deleting = null;
		}
	}
</script>

<div use:reveal class="mx-auto w-full max-w-6xl space-y-6 pb-6">
	<!-- ===== Header ===================================================== -->
	<header class="flex flex-wrap items-end justify-between gap-x-6 gap-y-4">
		<div class="space-y-2">
			<h1 class="display text-foreground flex items-baseline gap-3 text-[40px]">
				{$t('dossiers.title')}
				{#if loaded}
					<span class="text-muted-foreground font-mono text-sm tracking-normal">{total}</span>
				{/if}
			</h1>
			<p class="text-muted-foreground text-sm">{$t('dossiers.subtitle')}</p>
		</div>
		<Button class="h-9 px-4" onclick={() => (createOpen = true)}>
			<Icon name="add-circle" class="h-4 w-4" />
			{$t('dossiers.new')}
		</Button>
	</header>

	<!-- ===== Toolbar ==================================================== -->
	<div class="flex flex-wrap items-center gap-2">
		<div class="relative">
			<Icon
				name="magnifier"
				class="text-muted-foreground pointer-events-none absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2"
			/>
			<Input
				bind:value={query}
				placeholder={$t('dossiers.searchPlaceholder')}
				class="bg-card h-9 w-64 pl-9 text-ui"
			/>
		</div>

		<Select.Root type="single" bind:value={matterFilter}>
			<Select.Trigger class="bg-card h-9 w-[210px] text-ui">
				{matterFilter === 'all' ? $t('dossiers.matterType') : matterLabel(matterFilter)}
			</Select.Trigger>
			<Select.Content>
				<Select.Item value="all">{$t('dossiers.filterAll')}</Select.Item>
				{#each matterTypes as type}
					<Select.Item value={type}>{$t(`dossiers.matterTypes.${type}`)}</Select.Item>
				{/each}
			</Select.Content>
		</Select.Root>

		<!-- State, as a segmented control -->
		<div class="bg-muted/70 inline-flex h-9 items-center gap-0.5 rounded-lg p-1">
			{#each ['all', ...STATES] as s}
				<button
					type="button"
					onclick={() => (stateFilter = s as typeof stateFilter)}
					class="h-7 cursor-pointer rounded-md px-3 text-xs font-medium transition-all {stateFilter ===
					s
						? 'bg-card text-foreground shadow-sm'
						: 'text-muted-foreground hover:text-foreground'}"
				>
					{s === 'all' ? $t('dossiers.filterAll') : $t(`dossiers.state.${s}`)}
				</button>
			{/each}
		</div>

		{#if filterCount > 0}
			<Button
				variant="ghost"
				size="sm"
				class="text-muted-foreground hover:text-foreground h-9 px-3 text-ui"
				onclick={resetFilters}
			>
				<Icon name="filter" class="h-4 w-4" />
				{$t('common.clear')}
			</Button>
		{/if}
	</div>

	<!-- ===== Table ====================================================== -->
	<div class="bg-card overflow-hidden rounded-2xl border shadow-xs">
		{#if !loaded}
			<div class="space-y-2 p-4">
				{#each { length: pageSize } as _}
					<Skeleton class="h-12 w-full rounded-xl" />
				{/each}
			</div>
		{:else}
			{#if $dossiers.error}
				<div class="text-destructive px-5 pt-4 text-xs">{$t('dossiers.loadError')}</div>
			{/if}
			<Table.Root class="[&_thead]:bg-transparent">
				<Table.Header class="bg-transparent">
					<Table.Row class="border-border hover:bg-transparent">
						{#each [['dossiers.name', 'pl-5'], ['dossiers.clientReference', ''], ['dossiers.matterType', ''], ['dossiers.filterState', ''], ['dossiers.updatedAt', '']] as [key, cls]}
							<Table.Head
								class="text-muted-foreground h-10 text-[10px] font-medium tracking-[0.1em] whitespace-nowrap uppercase {cls}"
							>
								{$t(key)}
							</Table.Head>
						{/each}
						<Table.Head class="w-[1%]"></Table.Head>
					</Table.Row>
				</Table.Header>
				<Table.Body>
					{#each rows as dossier (dossier.id)}
						<Table.Row
							class="group/row border-border/50 hover:bg-muted/40 cursor-pointer transition-colors last:border-0"
							onclick={() => goto(`/dossiers/${dossier.id}`)}
						>
							<Table.Cell class="pl-5">
								<div class="flex items-center gap-3">
									<span
										class="bg-muted text-muted-foreground group-hover/row:bg-brand-soft group-hover/row:text-brand flex h-8 w-8 shrink-0 items-center justify-center rounded-lg transition-colors"
									>
										<Icon name="folder" class="h-4 w-4" />
									</span>
									<a
										href={`/dossiers/${dossier.id}`}
										class="text-foreground text-sm font-medium"
										onclick={(e) => e.stopPropagation()}
									>
										{dossier.title}
									</a>
								</div>
							</Table.Cell>
							<Table.Cell class="text-muted-foreground font-mono text-xs">
								{dossier.client_reference || '—'}
							</Table.Cell>
							<Table.Cell class="text-muted-foreground text-ui">
								{matterLabel(dossier.matter_type)}
							</Table.Cell>
							<Table.Cell>
								<DossierStateChip state={dossier.state} />
							</Table.Cell>
							<Table.Cell class="text-muted-foreground font-mono text-xs tabular-nums">
								{formatDate(dossier.updated_at)}
							</Table.Cell>
							<Table.Cell class="w-[1%] pr-4 whitespace-nowrap">
								<div
									class="flex items-center justify-end gap-0.5 opacity-0 transition-opacity group-hover/row:opacity-100 focus-within:opacity-100"
								>
									<Tooltip.Root>
										<Tooltip.Trigger>
											{#snippet child({ props })}
												<Button
													{...props}
													variant="ghost"
													size="icon"
													class="text-muted-foreground hover:text-destructive hover:bg-destructive/10 h-8 w-8"
													disabled={deleting === dossier.id}
													onclick={(e) => {
														e.stopPropagation();
														remove(dossier);
													}}
													aria-label={$t('dossiers.delete')}
												>
													<Icon name="trash-bin-trash" class="h-4 w-4" />
												</Button>
											{/snippet}
										</Tooltip.Trigger>
										<Tooltip.Content>{$t('dossiers.delete')}</Tooltip.Content>
									</Tooltip.Root>
									<Icon name="arrow-right" class="text-muted-foreground ml-1 h-4 w-4" />
								</div>
							</Table.Cell>
						</Table.Row>
					{:else}
						<Table.Row class="hover:bg-transparent">
							<Table.Cell colspan={6} class="p-0">
								<EmptyState
									icon={filterCount > 0 ? 'magnifier-zoom-out' : 'folder'}
									title={filterCount > 0 ? $t('common.noResults') : $t('dossiers.none')}
									hint={filterCount > 0 ? '' : $t('dossiers.noneHint')}
								>
									{#if filterCount > 0}
										<Button variant="ghost" size="sm" onclick={resetFilters}>
											<Icon name="filter" class="h-4 w-4" />
											{$t('common.clear')}
										</Button>
									{:else}
										<Button size="sm" onclick={() => (createOpen = true)}>
											<Icon name="add-circle" class="h-4 w-4" />
											{$t('dossiers.new')}
										</Button>
									{/if}
								</EmptyState>
							</Table.Cell>
						</Table.Row>
					{/each}
				</Table.Body>
			</Table.Root>

			<!-- ===== Pagination footer ================================== -->
			<div
				class="border-border bg-muted/30 flex flex-col gap-3 border-t px-5 py-3 text-xs sm:flex-row sm:items-center sm:justify-between"
			>
				<div class="text-muted-foreground">
					{$t('common.showingOf', tp({ count: `${pageInfo.start}–${pageInfo.end}`, total: filtered.length }))}
				</div>
				<div class="flex items-center gap-1">
					<span class="text-muted-foreground mr-2 font-mono tabular-nums">
						{pageIndex + 1} / {totalPages}
					</span>
					{#each [{ icon: 'double-alt-arrow-left', to: 0, off: pageIndex === 0, label: 'common.previous' }, { icon: 'alt-arrow-left', to: Math.max(0, pageIndex - 1), off: pageIndex === 0, label: 'common.previous' }, { icon: 'alt-arrow-right', to: Math.min(totalPages - 1, pageIndex + 1), off: pageIndex >= totalPages - 1, label: 'common.next' }, { icon: 'double-alt-arrow-right', to: totalPages - 1, off: pageIndex >= totalPages - 1, label: 'common.next' }] as nav}
						<Button
							variant="ghost"
							size="icon"
							class="h-8 w-8"
							onclick={() => (pageIndex = nav.to)}
							disabled={nav.off}
							aria-label={$t(nav.label)}
						>
							<Icon name={nav.icon as 'alt-arrow-left'} class="h-4 w-4" />
						</Button>
					{/each}
				</div>
			</div>
		{/if}
	</div>
</div>

<DossierCreateSheet bind:open={createOpen} oncreated={created} />

<ConfirmDialog
	bind:open={deleteOpen}
	title={$t('dossiers.deleteTitle')}
	description={$t('dossiers.deleteBody')}
	details={[
		$t('dossiers.deletePieces'),
		$t('dossiers.deleteExtracts'),
		$t('dossiers.deleteRuns'),
		$t('dossiers.deleteKeepsThreads')
	]}
	confirmLabel={$t('dossiers.delete')}
	onconfirm={confirmRemove}
/>
