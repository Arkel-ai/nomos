<script lang="ts">
	import * as Accordion from '$lib/components/ui/accordion';
	import Icon from '$lib/components/icon.svelte';
	import { t, locale } from '$lib/i18n';
	import { Button } from '$lib/components/ui/button';
	import Spinner from '$lib/components/spinner.svelte';
	import { getPieceDownloadLink } from '$lib/api/pieces';
	import { getCitation, formatLocator } from '$lib/api/citations';
	import type { AgentCitation, CitationCheck, SourceLocator } from '$lib/api/agent-contracts';
	import { isAgentCitation, type ThreadCitation } from './types';
	import CitationDetail from './CitationDetail.svelte';

	let { citations, total, expanded = $bindable(false), nextAfter = null, loading = false, error = false, onmore, onretry, oninspect, loadDetail = getCitation }: {
		citations: ThreadCitation[];
		/** Every source of the run, loaded or not; defaults to the loaded ones. */
		total?: number;
		/** Collapsed by default: the list opens on demand or from a [n] reference. */
		expanded?: boolean;
		nextAfter?: number | null;
		loading?: boolean;
		error?: boolean;
		onmore?: () => void;
		oninspect?: () => void;
		onretry?: () => void;
		/** A source reader can be supplied in place of the API (tests, isolated renders). */
		loadDetail?: typeof getCitation;
	} = $props();
	let selected = $state<ThreadCitation | null>(null);
	let open = $state(false);
	let trigger = $state<HTMLElement | null>(null);
	let downloading = $state<string | null>(null);
	let downloadError = $state<string | null>(null);
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);
	const number = (n: number) => new Intl.NumberFormat($locale || 'fr').format(n);
	const date = (seconds: number) => new Date(seconds * 1000).toLocaleDateString($locale || 'fr', { year: 'numeric', month: 'short', day: 'numeric' });
	const knownTypes = new Set(['piece', 'legifrance', 'judilibre', 'web']);
	const ordered = $derived([...citations].sort((a, b) => (isAgentCitation(a) ? a.number : 0) - (isAgentCitation(b) ? b.number : 0)));

	function iconFor(citation: ThreadCitation) {
		return citation.source_type === 'legifrance' ? 'scale' : citation.source_type === 'judilibre' ? 'library' : citation.source_type === 'web' ? 'global' : 'document-text';
	}
	function inspect(citation: ThreadCitation, event: MouseEvent) {
		oninspect?.();
		selected = citation;
		trigger = event.currentTarget as HTMLElement;
		open = true;
	}

	const locator = (value: SourceLocator | string | undefined) => formatLocator(value, tx, number);
	function checkLabel(check: CitationCheck): string {
		if (check.check === 'official_matched' && check.status === 'unavailable') return $t('citations.officialUnavailable');
		if (check.status === 'passed') return $t(`citations.checkPassed.${check.check}`);
		return `${$t(`citations.checkNames.${check.check}`)} · ${$t(`citations.checkStatus.${check.status}`)}`;
	}
	function sourceURL(citation: ThreadCitation): string | undefined {
		const url = 'url' in citation ? citation.url : 'canonical_url' in citation ? citation.canonical_url : undefined;
		// Stored external URLs are data, never an executable navigation scheme.
		return url && /^https?:\/\//i.test(url) ? url : undefined;
	}
	async function openPiece(citation: AgentCitation) {
		if (citation.source_type !== 'piece') return;
		downloading = citation.id;
		downloadError = null;
		try {
			const link = await getPieceDownloadLink(citation.piece_id);
			const anchor = document.createElement('a');
			anchor.href = link.url;
			anchor.download = link.name;
			anchor.rel = 'noopener noreferrer';
			anchor.click();
		} catch { downloadError = citation.id; }
		finally { downloading = null; }
	}
</script>

{#snippet source(citation: ThreadCitation)}
	<div class="min-w-0 space-y-3 [overflow-wrap:anywhere]">
		<div class="flex flex-wrap items-center gap-2 text-2xs text-muted-foreground">
			<Icon name={iconFor(citation)} class="h-4 w-4 shrink-0" />
			<span>{$t(knownTypes.has(citation.source_type) ? `citations.types.${citation.source_type}` : 'citations.source')}</span>
			{#if isAgentCitation(citation)}<span class="font-mono tabular-nums">[{number(citation.number)}]</span>{/if}
		</div>
		<p class="truncate text-sm font-medium" title={citation.label}>{citation.label}</p>
		{#if locator(citation.locator)}<p class="break-words font-mono text-2xs tabular-nums text-muted-foreground">{locator(citation.locator)}</p>{/if}
		<blockquote class="whitespace-pre-wrap break-words border-l-2 border-border pl-3 font-sans text-read leading-relaxed">{citation.quote}</blockquote>
		{#if isAgentCitation(citation)}
			<ul class="space-y-1.5">
				{#each citation.checks ?? [] as check, i (i)}
					<li class="flex items-start gap-2 rounded-lg px-2 py-1 text-xs {check.status === 'passed' ? 'bg-pos-soft text-pos-soft-foreground' : 'text-muted-foreground'}">
						<Icon name={check.status === 'passed' ? 'check-circle' : check.status === 'failed' ? 'danger-triangle' : 'info-circle'} active={check.status === 'passed'} class="mt-px h-3.5 w-3.5 shrink-0" />
						{checkLabel(check)}
					</li>
				{/each}
			</ul>
			{#if (citation.warning_codes ?? []).some((code) => code.startsWith('piece_'))}
				<p class="rounded-lg bg-amber-soft p-2 text-xs text-amber-soft-foreground">{$t('citations.pieceMismatch')}</p>
			{/if}
			<div class="flex flex-wrap gap-x-3 gap-y-1 text-2xs text-muted-foreground">
				{#if citation.source_type === 'piece'}
					{#if citation.piece_ordinal !== undefined}<span class="font-mono tabular-nums">{tx('citations.pieceOrdinal', { number: number(citation.piece_ordinal) })}</span>{/if}
					{#if citation.filename}<span class="min-w-0 truncate" title={citation.filename}>{citation.filename}</span>{/if}
				{:else if citation.source_type === 'legifrance'}
					{#if citation.article_label}<span>{citation.article_label}</span>{/if}
					{#if citation.valid_from !== undefined}<span class="font-mono">{citation.valid_to !== undefined ? tx('citations.version', { from: date(citation.valid_from), to: date(citation.valid_to) }) : tx('citations.versionOpenEnded', { from: date(citation.valid_from) })}</span>{/if}
				{:else if citation.source_type === 'judilibre'}
					{#if citation.court}<span>{citation.court}</span>{/if}
					{#if citation.chamber}<span>{citation.chamber}</span>{/if}
					{#if citation.decision_date !== undefined}<span class="font-mono">{date(citation.decision_date)}</span>{/if}
					{#if citation.ecli}<span class="font-mono">{citation.ecli}</span>{/if}
					{#if citation.pourvoi}<span class="font-mono">{citation.pourvoi}</span>{/if}
				{:else if citation.source_type === 'web'}
					{#if citation.host}<span>{citation.host}</span>{/if}
					<span class="break-words">{citation.url}</span>
					{#if citation.extent === 'excerpt'}<span>{$t('citations.webExcerpt')}</span>{/if}
				{/if}
				{#if citation.retrieved_at}<span>{$t('citations.retrievedAt')} <span class="font-mono">{date(citation.retrieved_at)}</span></span>{/if}
				{#if citation.verified_by}<span>{$t('citations.verifiedBy')} {citation.verified_by}</span>{/if}
			</div>
		{/if}
		{#if isAgentCitation(citation) && citation.source_type === 'piece' && citation.piece_id}
			<Button variant="ghost" size="sm" class="min-h-11 text-xs text-brand focus-visible:ring-2 focus-visible:ring-brand ring-offset-background" onclick={() => openPiece(citation)} disabled={downloading === citation.id}>
				{#if downloading === citation.id}<Spinner class="h-4 w-4" />{:else}<Icon name="download" class="h-4 w-4" />{/if}{$t('citations.openSourceLink')}
			</Button>
			{#if downloadError === citation.id}<p class="text-xs text-destructive">{$t('artifacts.downloadError')}</p>{/if}
		{:else if sourceURL(citation)}
			<a href={sourceURL(citation)} target="_blank" rel="noopener noreferrer" class="inline-flex min-h-11 items-center gap-1 rounded-lg text-xs text-brand focus-visible:ring-2 focus-visible:ring-brand ring-offset-background"><Icon name="square-top-down" class="h-4 w-4" />{$t('citations.openSourceLink')}</a>
		{/if}
	</div>
{/snippet}

<section class="min-w-0 rounded-2xl border border-border bg-card px-4" aria-label={$t('citations.title')}>
	<Accordion.Root type="single" bind:value={() => (expanded ? 'sources' : ''), (value) => (expanded = value === 'sources')}>
		<Accordion.Item value="sources" class="border-0">
			<Accordion.Trigger class="min-h-11 items-center gap-3 py-3 hover:no-underline focus-visible:ring-2 focus-visible:ring-brand ring-offset-background">
				<span class="flex min-w-0 flex-1 items-center gap-2.5">
					<span class="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground" aria-hidden="true">
						<Icon name="book-bookmark" active class="h-3.5 w-3.5" />
					</span>
					<span class="text-sm font-medium">{$t('citations.title')}</span>
					<span class="font-mono text-xs font-normal tabular-nums text-muted-foreground">{number(total ?? citations.length)}</span>
				</span>
			</Accordion.Trigger>
			<Accordion.Content>
				<ol class="divide-y divide-border/70 border-t border-border/70 pb-1">
					{#each ordered as citation (citation.id)}
						<li>
							<button
								id="citation-{citation.id}"
								type="button"
								class="-mx-2 flex min-h-11 w-[calc(100%+1rem)] min-w-0 items-center gap-2 rounded-lg px-2 py-2 text-left transition-colors duration-200 hover:bg-muted/60 focus-visible:ring-2 focus-visible:ring-brand ring-offset-background"
								aria-label="{citation.label} — {$t('citations.viewPassage')}"
								onclick={(event) => inspect(citation, event)}
							>
								<span class="inline-flex h-6 min-w-6 shrink-0 items-center justify-center rounded-md bg-muted px-1.5 font-mono text-2xs tabular-nums text-muted-foreground">{#if isAgentCitation(citation)}{number(citation.number)}{/if}</span>
								<Icon name={iconFor(citation)} class="h-4 w-4 shrink-0 text-muted-foreground" />
								<span class="min-w-0 flex-1 truncate text-ui" title={citation.label}>{citation.label}</span>
								{#if isAgentCitation(citation) && (citation.warning_codes ?? []).some((code) => code.startsWith('piece_'))}
									<span class="h-1.5 w-1.5 shrink-0 rounded-full bg-amber-soft-foreground" title={$t('citations.pieceMismatch')}></span>
								{/if}
								{#if citation.locator}<span class="hidden max-w-[40%] shrink-0 truncate font-mono text-2xs tabular-nums text-muted-foreground sm:inline">{locator(citation.locator)}</span>{/if}
							</button>
						</li>
					{/each}
				</ol>
				{#if error}
					<p class="text-xs text-muted-foreground">{$t('citations.loadError')}</p>
					<Button variant="ghost" size="sm" disabled={loading} onclick={onretry}>{$t('citations.retryDisplay')}</Button>
				{:else if nextAfter !== null}
					<Button variant="ghost" size="sm" disabled={loading} onclick={onmore}>{#if loading}<Spinner class="h-4 w-4" />{/if}{$t('citations.showMore')}</Button>
				{/if}
			</Accordion.Content>
		</Accordion.Item>
	</Accordion.Root>
</section>

{#if selected}
	<CitationDetail citation={selected} bind:open returnFocus={trigger} content={source} load={loadDetail} />
{/if}
