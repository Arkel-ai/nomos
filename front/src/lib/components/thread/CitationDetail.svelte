<script lang="ts">
	import type { Snippet } from 'svelte';
	import * as Sheet from '$lib/components/ui/sheet';
	import { Button } from '$lib/components/ui/button';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import Icon from '$lib/components/icon.svelte';
	import { t, locale } from '$lib/i18n';
	import { getCitation, formatLocator } from '$lib/api/citations';
	import type { AgentCitationDetail } from '$lib/api/agent-contracts';
	import type { ThreadCitation } from './types';

	let { citation, open = $bindable(false), returnFocus = null, content, load = getCitation }: {
		citation: ThreadCitation;
		open?: boolean;
		returnFocus?: HTMLElement | null;
		content: Snippet<[ThreadCitation]>;
		load?: typeof getCitation;
	} = $props();
	let detail = $state<AgentCitationDetail | null>(null);
	let loading = $state(false);
	let error = $state(false);
	let request = 0;
	async function refresh() {
		const own = ++request;
		loading = true;
		error = false;
		try {
			const result = await load(citation.id);
			if (own === request) detail = result;
		} catch { if (own === request) error = true; }
		finally { if (own === request) loading = false; }
	}
	$effect(() => {
		if (open) { void citation.id; detail = null; void refresh(); }
		return () => { request++; };
	});
	// The exact place the passage is cited from: § 4, Feuille!B4, Page 2 · § 3.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);
	const where = $derived(formatLocator(citation.locator, tx, (n) => new Intl.NumberFormat($locale || 'fr').format(n)));
	// The wire offsets count Unicode code points, not UTF-16 code units.
	const passage = $derived(Array.from(detail?.passage ?? ''));
	const start = $derived(Math.max(0, Math.min(passage.length, detail?.highlight_start ?? 0)));
	const end = $derived(Math.max(start, Math.min(passage.length, detail?.highlight_end ?? start)));
</script>

<Sheet.Root bind:open>
	<Sheet.Content side="right" class="citation-sheet w-full min-w-0 gap-0 sm:max-w-xl" onCloseAutoFocus={(event) => { event.preventDefault(); returnFocus?.focus({ preventScroll: true }); }}>
		<Sheet.Header class="shrink-0 border-b border-border p-4 pr-16 text-left">
			<Sheet.Title class="truncate text-sm font-medium" title={citation.label}>{citation.label}</Sheet.Title>
			<Sheet.Description class="text-xs">{$t('citations.viewPassage')}{#if where} · <span class="font-mono tabular-nums">{where}</span>{/if}</Sheet.Description>
		</Sheet.Header>
		<Sheet.Close class="absolute right-2 top-2 flex h-11 w-11 items-center justify-center rounded-lg text-muted-foreground focus-visible:ring-2 focus-visible:ring-brand ring-offset-background" aria-label={$t('citations.close')}>
			<Icon name="close-circle" class="h-5 w-5" />
		</Sheet.Close>
		<div class="min-h-0 flex-1 space-y-4 overflow-y-auto p-4 [overflow-wrap:anywhere]">
			{#if loading}
				<div class="space-y-4 rounded-2xl border border-border p-4" aria-busy="true" aria-label={$t('citations.loading')}>
					<Skeleton class="h-4 w-24" /><Skeleton class="h-5 w-3/4" /><Skeleton class="h-20 w-full" /><Skeleton class="h-4 w-2/3" /><Skeleton class="h-11 w-36" />
				</div>
			{:else}
				{@render content(detail ?? citation)}
				{#if error}
					<p class="text-xs text-muted-foreground">{$t('citations.loadError')}</p>
					<Button variant="outline" onclick={refresh}>{$t('citations.retryDisplay')}</Button>
				{:else if detail}
					{#if !detail.source_available}<p class="rounded-lg bg-muted p-3 text-xs text-muted-foreground">{$t('citations.unavailable')}</p>{/if}
					{#if detail.passage}
						<p class="whitespace-pre-wrap break-words text-read leading-relaxed">{passage.slice(0, start).join('')}<mark class="rounded-sm bg-brand-soft text-brand-soft-foreground">{passage.slice(start, end).join('')}</mark>{passage.slice(end).join('')}</p>
					{/if}
				{/if}
			{/if}
		</div>
	</Sheet.Content>
</Sheet.Root>
