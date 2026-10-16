<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// =====================================================================
	// /search — manual legal search (docs/PLAN.md §6, §10, §13).
	//
	// The list-page layout: one Card.Root, icon + title + inline
	// count on the left, controls on the right, the canonical table styling,
	// the canonical empty state.
	//
	// Three things this page must never do:
	//   - claim a result was reranked when `reranked` is false;
	//   - present relevance as precedential weight — RRF and a reranker say
	//     nothing about a revirement, a distinction or an abrogation (§6.3);
	//   - show a hit without its source and its licence. Licence Ouverte 2.0
	//     permits reuse WITH attribution, so attribution is a product
	//     requirement, not a footer (§13).
	// =====================================================================

	import { onMount } from 'svelte';
	import { fly } from 'svelte/transition';
	import { cubicOut } from 'svelte/easing';
	import * as Card from '$lib/components/ui/card';
	import * as Table from '$lib/components/ui/table';
	import * as Select from '$lib/components/ui/select';
	import * as Popover from '$lib/components/ui/popover';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { Badge } from '$lib/components/ui/badge';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Button } from '$lib/components/ui/button';
	import { Separator } from '$lib/components/ui/separator';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { Calendar } from '$lib/components/ui/calendar';
	import EmptyState from '$lib/components/empty-state.svelte';
	import {
		CalendarDate,
		getLocalTimeZone,
		today,
		type DateValue
	} from '@internationalized/date';
	import { t } from '$lib/i18n';
	import { tp } from '$lib/components/dossier/i18n-payload';
	import { search, type Corpus, type SearchHit, type SearchResponse } from '$lib/api/search';
	import { dossiers, loadDossiers } from '$lib/stores/dossier';

	// ---- state ----------------------------------------------------------
	let query = $state('');
	let corpus = $state<Corpus>('public');
	let dossierId = $state('');
	let asOf = $state<DateValue | undefined>();
	let asOfOpen = $state(false);

	let running = $state(false);
	let ran = $state(false);
	let error = $state('');
	let response = $state<SearchResponse | null>(null);

	const results = $derived(response?.results ?? []);
	const needsDossier = $derived(corpus !== 'public');
	const canRun = $derived(
		query.trim().length > 1 && !running && (!needsDossier || dossierId !== '')
	);

	const corpusOptions: { value: Corpus; labelKey: string }[] = [
		{ value: 'public', labelKey: 'search.corpusOptions.public' },
		{ value: 'private', labelKey: 'search.corpusOptions.private' },
		{ value: 'both', labelKey: 'search.corpusOptions.both' }
	];
	const corpusLabel = $derived(
		$t(corpusOptions.find((o) => o.value === corpus)?.labelKey ?? 'search.corpusOptions.public')
	);
	const dossierLabel = $derived(
		$dossiers.items.find((d) => d.id === dossierId)?.title ?? $t('common.none')
	);

	onMount(() => {
		void loadDossiers();
	});

	// ---- formatting -----------------------------------------------------
	function formatUnix(seconds: number | null | undefined): string {
		if (!seconds) return '—';
		return new Date(seconds * 1000).toLocaleDateString('fr-FR', {
			year: 'numeric',
			month: '2-digit',
			day: '2-digit'
		});
	}

	function formatDateValue(d: DateValue | undefined): string {
		if (!d) return $t('search.today');
		return d.toDate(getLocalTimeZone()).toLocaleDateString('fr-FR', {
			year: 'numeric',
			month: 'long',
			day: 'numeric'
		});
	}

	/** The half-open interval of §6.4, rendered as a human range. */
	function validity(hit: SearchHit): string {
		if (!hit.valid_from && !hit.valid_to) return '—';
		const from = formatUnix(hit.valid_from);
		const to = hit.valid_to ? formatUnix(hit.valid_to) : $t('search.validityOpen');
		return `${from} → ${to}`;
	}

	function decisionDate(hit: SearchHit): string {
		return formatUnix(hit.decision_date ?? hit.valid_from ?? null);
	}

	function excerpt(text: string): string {
		const flat = text.replace(/\s+/g, ' ').trim();
		return flat.length > 260 ? `${flat.slice(0, 260)}…` : flat;
	}

	// ---- the query ------------------------------------------------------
	async function run() {
		if (!canRun) return;
		running = true;
		error = '';
		try {
			response = await search({
				query: query.trim(),
				corpus,
				dossier_id: needsDossier ? dossierId : null,
				as_of_date: asOf ? Math.floor(asOf.toDate(getLocalTimeZone()).getTime() / 1000) : 0
			});
			ran = true;
		} catch (err) {
			response = null;
			ran = true;
			error = (err as { message?: string }).message ?? $t('search.error');
		} finally {
			running = false;
		}
	}
</script>

<div in:fly={{ y: 12, duration: 400, easing: cubicOut }} class="mx-auto w-full max-w-6xl space-y-6 pb-6">
	<header class="space-y-2">
		<h1 class="display text-foreground flex items-baseline gap-3 text-[40px]">
			{$t('search.title')}
			{#if ran && response}
				<span class="text-muted-foreground font-mono text-sm tracking-normal">
					{response.results.length}
				</span>
			{/if}
		</h1>
		<p class="text-muted-foreground text-sm">{$t('search.subtitle')}</p>
	</header>

	<Card.Root class="gap-3 rounded-2xl p-5">
		<!-- ===== Scope ==================================================== -->
		<div class="flex flex-wrap items-center justify-end gap-3">

			<div class="flex flex-wrap items-center gap-2">
				<!-- Corpus -->
				<Select.Root type="single" bind:value={corpus}>
					<Select.Trigger class="h-8 w-[190px] text-[12.5px]">{corpusLabel}</Select.Trigger>
					<Select.Content>
						{#each corpusOptions as option}
							<Select.Item value={option.value}>{$t(option.labelKey)}</Select.Item>
						{/each}
					</Select.Content>
				</Select.Root>

				<!-- The dossier a private read is scoped to. -->
				{#if needsDossier}
					<Select.Root type="single" bind:value={dossierId}>
						<Select.Trigger class="h-8 w-[190px] text-[12.5px]">{dossierLabel}</Select.Trigger>
						<Select.Content>
							{#each $dossiers.items as d (d.id)}
								<Select.Item value={d.id}>{d.title}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
				{/if}

				<!-- As-of date: the applicable rule is the one in force at the
				     date of the facts, not today's (§6.4). -->
				<Popover.Root bind:open={asOfOpen}>
					<Popover.Trigger>
						{#snippet child({ props })}
							<Button {...props} variant="outline" size="sm" class="h-8 gap-1.5 text-[12px]">
								<Icon name="calendar" class="h-3.5 w-3.5 opacity-70" />
								{formatDateValue(asOf)}
							</Button>
						{/snippet}
					</Popover.Trigger>
					<Popover.Content class="w-auto overflow-hidden p-0" align="end">
						<Calendar
							type="single"
							bind:value={asOf}
							captionLayout="dropdown"
							maxValue={today(getLocalTimeZone())}
							minValue={new CalendarDate(1800, 1, 1)}
							onValueChange={() => (asOfOpen = false)}
						/>
						<div class="border-border/60 flex justify-end border-t px-2 py-1.5">
							<Button
								variant="ghost"
								size="sm"
								class="text-muted-foreground h-7 text-xs"
								onclick={() => {
									asOf = undefined;
									asOfOpen = false;
								}}
							>
								<Icon name="close-circle" class="mr-1 h-3.5 w-3.5" />
								{$t('common.clear')}
							</Button>
						</div>
					</Popover.Content>
				</Popover.Root>
			</div>
		</div>

		<!-- ===== Query =================================================== -->
		<form
			class="contents"
			onsubmit={(e) => {
				e.preventDefault();
				run();
			}}
		>
			<div class="flex flex-wrap items-end gap-2">
				<div class="min-w-[260px] flex-1 space-y-1.5">
					<Label for="q" class="text-muted-foreground text-[11.5px]">{$t('search.title')}</Label>
					<div class="relative">
						<Icon name="magnifier"
							class="text-muted-foreground pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2"
						/>
						<Input
							id="q"
							bind:value={query}
							placeholder={$t('search.placeholder')}
							class="h-9 pl-8 text-[12.5px]"
						/>
					</div>
				</div>
				<Button type="submit" size="sm" class="h-9" disabled={!canRun}>
					{#if running}
						<Spinner class="mr-1 h-3.5 w-3.5" />
					{:else}
						<Icon name="magnifier" class="mr-1 h-3.5 w-3.5" />
					{/if}
					{running ? $t('search.searching') : $t('search.run')}
				</Button>
			</div>
		</form>

		<div class="text-muted-foreground/80 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px]">
			<span class="inline-flex items-center gap-1.5">
				<Icon name="calendar" class="h-3 w-3" />
				{$t('search.asOfDateHint')}
			</span>
			{#if needsDossier && dossierId === ''}
				<span class="text-destructive">{$t('search.dossierRequired')}</span>
			{/if}
		</div>

		{#if error}
			<p class="text-destructive text-[12.5px]">{error}</p>
		{/if}

		<Separator class="border-dashed" />

		<!-- ===== Results ================================================== -->
		{#if running}
			<div class="space-y-2 py-2">
				{#each { length: 6 } as _}
					<Skeleton class="h-8 w-full" />
				{/each}
			</div>
		{:else if !ran}
			<EmptyState icon="magnifier" title={$t('search.title')} hint={$t('search.emptyHint')} />
		{:else}
			<div class="flex flex-wrap items-center gap-2 text-[11px]">
				<Badge variant={response?.reranked ? 'default' : 'outline'}>
					{response?.reranked ? $t('search.reranked') : $t('search.notReranked')}
				</Badge>
				{#if response}
					<span class="text-muted-foreground tabular-nums">
						{$t('search.results', tp({ count: response.results.length }))}
					</span>
					{#if response.candidates}
						<span class="text-muted-foreground/70 tabular-nums">
							· {$t('search.candidates', tp({ count: response.candidates }))}
						</span>
					{/if}
					{#if response.as_of_date}
						<span class="text-muted-foreground/70">
							· {$t('search.asOfDate')} {formatUnix(response.as_of_date)}
						</span>
					{/if}
				{/if}
			</div>

			<div class="overflow-x-auto">
				<Table.Root class="[&_thead]:bg-transparent">
					<Table.Header class="bg-transparent">
						<Table.Row class="border-0 hover:bg-transparent">
							{#each [{ key: 'search.columns.source', icon: 'bookmark-square' as const }, { key: 'search.columns.jurisdiction', icon: 'buildings-3' as const }, { key: 'search.columns.formation', icon: 'scale' as const }, { key: 'search.columns.date', icon: 'calendar' as const }, { key: 'search.columns.publication', icon: 'file-text' as const }, { key: 'search.columns.validity', icon: 'calendar' as const }] as col}
								<Table.Head class="whitespace-nowrap">
									<span
										class="text-muted-foreground inline-flex h-7 items-center gap-1.5 text-[10px] font-semibold uppercase tracking-wider"
									>
										<Icon name={col.icon} class="text-muted-foreground/70 h-3.5 w-3.5" />
										{$t(col.key)}
									</span>
								</Table.Head>
							{/each}
							<Table.Head class="w-[1%]"><span></span></Table.Head>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{#each results as hit (`${hit.corpus}-${hit.chunk_id}`)}
							<Table.Row
								class="border-border/80 hover:bg-muted/40 border-dashed align-top transition-colors"
							>
								<!-- Source + licence: visible on EVERY hit (§13). -->
								<Table.Cell class="max-w-[420px] py-3">
									<div class="space-y-1">
										<div class="text-[12.5px] font-semibold">
											{hit.citation_label ||
												(hit.corpus === 'private'
													? $t('citations.pieceSource')
													: $t('citations.legalSource'))}
										</div>
										<div class="text-muted-foreground text-[11.5px] leading-relaxed">
											{excerpt(hit.text)}
										</div>
										<div class="flex flex-wrap items-center gap-1.5 pt-0.5">
											<Badge variant="outline" class="text-[10px]">
												{hit.corpus === 'private'
													? $t('search.corpusOptions.private')
													: $t('search.corpusOptions.public')}
											</Badge>
											{#if hit.source}
												<Badge variant="secondary" class="text-[10px]">{hit.source}</Badge>
											{/if}
											{#if hit.license_code}
												<span class="text-muted-foreground/80 text-[10px]">
													{$t('citations.license')} · {hit.license_code}
												</span>
											{/if}
											{#if hit.piece_id && hit.page_from}
												<span class="text-muted-foreground/80 text-[10px]">
													{$t('search.pages', tp({ from: hit.page_from, to: hit.page_to ?? hit.page_from }))}
												</span>
											{/if}
										</div>
									</div>
								</Table.Cell>
								<Table.Cell class="text-muted-foreground text-[12.5px]">
									{hit.jurisdiction || '—'}
								</Table.Cell>
								<Table.Cell class="text-muted-foreground text-[12.5px]">
									{hit.formation || '—'}
								</Table.Cell>
								<Table.Cell class="text-muted-foreground text-[12.5px] tabular-nums">
									{decisionDate(hit)}
								</Table.Cell>
								<Table.Cell class="text-[12.5px]">
									{#if hit.publication_level}
										<Badge variant="secondary" class="text-[10px]">{hit.publication_level}</Badge>
									{:else}
										<span class="text-muted-foreground">—</span>
									{/if}
								</Table.Cell>
								<Table.Cell class="text-muted-foreground text-[11.5px] tabular-nums">
									{validity(hit)}
								</Table.Cell>
								<Table.Cell class="w-[1%] whitespace-nowrap">
									<div class="flex items-center justify-end gap-0.5">
										<Tooltip.Root>
											<Tooltip.Trigger>
												<span class="text-muted-foreground/80 px-1 text-[11px] tabular-nums">
													{hit.score.toFixed(3)}
												</span>
											</Tooltip.Trigger>
											<Tooltip.Content>{$t('search.score')}</Tooltip.Content>
										</Tooltip.Root>
										{#if hit.canonical_url}
											<Tooltip.Root>
												<Tooltip.Trigger>
													<Button
														variant="ghost"
														size="icon"
														class="text-muted-foreground hover:text-foreground h-7 w-7"
														href={hit.canonical_url}
														target="_blank"
														rel="noreferrer"
														aria-label={$t('citations.openSource')}
													>
														<Icon name="square-top-down" class="h-3.5 w-3.5" />
													</Button>
												</Tooltip.Trigger>
												<Tooltip.Content>{$t('citations.openSource')}</Tooltip.Content>
											</Tooltip.Root>
										{/if}
									</div>
								</Table.Cell>
							</Table.Row>
						{:else}
							<Table.Row class="hover:bg-transparent">
								<Table.Cell colspan={7} class="p-0">
									<EmptyState icon="magnifier-zoom-out" title={$t('search.none')} hint={$t('search.emptyHint')} />
								</Table.Cell>
							</Table.Row>
						{/each}
					</Table.Body>
				</Table.Root>
			</div>

			<p
				class="text-muted-foreground/80 border-border/60 flex items-start gap-1.5 border-t pt-3 text-[11px]"
			>
				<Icon name="info-circle" class="mt-px h-3 w-3 shrink-0" />
				{$t('citations.disclaimer')}
			</p>
		{/if}
	</Card.Root>

	<!-- ===== Attribution — a product requirement, never a footer ========= -->
	{#if response && response.attribution?.length}
		<Card.Root class="gap-3 rounded-xl p-5">
			<div class="text-muted-foreground flex items-center gap-1.5 text-[12.5px] font-medium">
				<Icon name="bookmark-square" class="h-3.5 w-3.5 opacity-80" />
				{$t('search.attributionTitle')}
			</div>
			<div class="space-y-2">
				{#each response.attribution as credit (credit.source)}
					<div
						class="border-border/80 flex flex-wrap items-center justify-between gap-2 rounded-md border border-dashed p-3"
					>
						<div class="space-y-0.5">
							<div class="text-[12.5px] font-medium">{credit.source}</div>
							<div class="text-muted-foreground text-[11.5px]">{credit.notice}</div>
						</div>
						<div class="flex items-center gap-2">
							{#if credit.license_code}
								<Badge variant="outline" class="text-[10px]">{credit.license_code}</Badge>
							{/if}
							<span class="text-muted-foreground/70 text-[11px] tabular-nums">
								{$t('search.hits', tp({ count: credit.hits }))}
							</span>
						</div>
					</div>
				{/each}
			</div>
			<p class="text-muted-foreground/80 text-[11px]">{$t('search.attributionHint')}</p>
		</Card.Root>
	{/if}
</div>
