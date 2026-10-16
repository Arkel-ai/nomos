<script lang="ts">
	import * as Accordion from '$lib/components/ui/accordion';
	import Icon from '$lib/components/icon.svelte';
	import { t, locale } from '$lib/i18n';
	import type { CoverageReport } from '$lib/api/agent-contracts';
	let { coverage, expanded = false }: { coverage: CoverageReport; expanded?: boolean } = $props();
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);
	const number = (value: number) => new Intl.NumberFormat($locale || 'fr').format(value);
	const missing = $derived(Math.max(0, coverage.total_pieces - coverage.fully_read));
	const ratio = $derived(coverage.total_pieces ? Math.min(1, coverage.fully_read / coverage.total_pieces) : 0);
	// `complete` is the server's verdict from read receipts, never inferred here.
	const status = $derived(
		coverage.complete
			? { key: 'coverage.chip.complete', tone: 'bg-pos-soft text-pos-soft-foreground' }
			: coverage.segments.unreadable > 0
				? { key: 'coverage.chip.unreadable', tone: 'bg-amber-soft text-amber-soft-foreground' }
				: coverage.segments.pending > 0
					? { key: 'coverage.chip.pending', tone: 'bg-muted text-muted-foreground' }
					: { key: 'coverage.chip.partial', tone: 'bg-amber-soft text-amber-soft-foreground' }
	);
</script>

<section class="min-w-0 rounded-2xl border border-border bg-card px-4" aria-label={$t('coverage.title')}>
	<Accordion.Root type="single" value={expanded ? 'pieces' : undefined}>
		<Accordion.Item value="pieces" class="border-0">
			<Accordion.Trigger class="min-h-11 items-center gap-3 py-3 hover:no-underline focus-visible:ring-2 focus-visible:ring-brand ring-offset-background">
				<span class="flex min-w-0 flex-1 flex-wrap items-center gap-x-2.5 gap-y-1.5">
					<span class="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground" aria-hidden="true">
						<Icon name="library" active class="h-3.5 w-3.5" />
					</span>
					<span class="text-sm font-medium">{$t('coverage.title')}</span>
					<span class="font-mono text-xs font-normal tabular-nums text-muted-foreground" aria-hidden="true">{number(coverage.fully_read)}/{number(coverage.total_pieces)}</span>
					<span class="sr-only">{tx('coverage.count', { read: number(coverage.fully_read), total: number(coverage.total_pieces) })}</span>
					<span class="inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-2xs font-normal sm:ml-auto {status.tone}">
						<span class="h-1.5 w-1.5 rounded-full bg-current" aria-hidden="true"></span>{tx(status.key, { missing: number(missing) })}
					</span>
				</span>
			</Accordion.Trigger>
			<div class="pb-3.5" aria-hidden="true">
				<div class="h-1 overflow-hidden rounded-full bg-muted">
					<div class="h-full rounded-full transition-[width] duration-700 ease-out {coverage.complete ? 'bg-pos' : 'bg-muted-foreground/45'}" style="width: {ratio * 100}%"></div>
				</div>
			</div>
			<Accordion.Content>
				<ol class="divide-y divide-border/70 border-t border-border/70">
					{#each coverage.pieces as piece (piece.piece_id)}
						<li class="grid min-w-0 grid-cols-[2rem_minmax(0,1fr)] items-start gap-x-2 gap-y-1 py-2.5 sm:grid-cols-[2rem_minmax(0,1fr)_auto] sm:items-center">
							<span class="pt-0.5 font-mono text-2xs tabular-nums text-muted-foreground sm:pt-0">{number(piece.ordinal)}</span>
							<div class="min-w-0">
								<p class="truncate text-ui" title={piece.name}>{piece.name}</p>
								<p class="font-mono text-2xs tabular-nums text-muted-foreground">{tx(`coverage.units.${piece.unit}`, { read: number(piece.segments.consumed), total: number(piece.segments.expected) })}</p>
							</div>
							<!-- A whole read is the norm and stays quiet; only exceptions get a chip. -->
							<span class="col-start-2 w-fit rounded-full py-0.5 text-2xs sm:col-start-auto {piece.state === 'unreadable' || piece.mutated ? 'bg-amber-soft px-2 text-amber-soft-foreground' : piece.state === 'complete' ? 'text-muted-foreground' : 'bg-muted px-2 text-muted-foreground'}">
								<span class="mr-1 inline-block h-1.5 w-1.5 rounded-full {piece.state === 'complete' && !piece.mutated ? 'bg-pos' : 'bg-current'}" aria-hidden="true"></span>{$t(`coverage.states.${piece.state}`)}
							</span>
							{#if piece.mutated}<p class="col-start-2 text-2xs text-amber-soft-foreground">{$t('coverage.mutated')}</p>{/if}
						</li>
					{/each}
				</ol>
			</Accordion.Content>
		</Accordion.Item>
	</Accordion.Root>
</section>
