<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import type { SolarIconName } from '$lib/icons/solar';
	import { t, locale } from '$lib/i18n';
	import { Button } from '$lib/components/ui/button';
	import { isTerminalRunState } from '$lib/api/runs';
	import type { RunView } from '$lib/stores/conversation';
	import type { ActivityObjectKind, AgentActivityCounts } from '$lib/api/agent-contracts';
	import AgentOrb from './AgentOrb.svelte';
	import ThinkingRow from './ThinkingRow.svelte';
	import StepGlyph from './StepGlyph.svelte';
	import { agentActivityKey, formatElapsed } from './activity';

	let { run, oncancel, cancelling = false }: {
		run: RunView;
		oncancel?: (id: string) => void;
		cancelling?: boolean;
	} = $props();
	let expanded = $state(false);
	const componentId = $props.id();
	const regionId = `${componentId}-activity`;
	const terminal = $derived(isTerminalRunState(run.state));
	const waiting = $derived(run.state === 'waiting_input');
	const rows = $derived(run.activity ?? []);
	const label = $derived($t(waiting ? 'chat.agent.waiting' : agentActivityKey(rows.at(-1)?.tool)));
	const groups = $derived.by(() => {
		const grouped = new Map<number, typeof rows>();
		for (const row of rows) grouped.set(row.dispatch, [...(grouped.get(row.dispatch) ?? []), row]);
		return [...grouped.entries()].sort(([a], [b]) => a - b);
	});
	const visible = $derived(terminal ? expanded ? groups : [] : groups.slice(-1));
	const icons: Record<ActivityObjectKind, SolarIconName> = {
		pieces: 'library', piece: 'document-text', query: 'magnifier', law: 'scale',
		decision: 'library', evidence: 'document-text', competence: 'square-academic-cap',
		question: 'question-circle', document: 'document-text', verification: 'shield-check',
		review: 'document-text', calculation: 'hashtag', artifact: 'archive', submission: 'check-circle'
	};
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);
	const fmt = $derived(new Intl.NumberFormat($locale || 'fr'));
	/** The delivered turn's summary: how many steps, over how long. */
	const summary = $derived.by(() => {
		const parts = [tx('chat.agent.steps', { count: fmt.format(groups.length) })];
		const last = rows.at(-1)?.at;
		if (last && last > run.created_at) parts.push(formatElapsed(last - run.created_at, $locale || 'fr', false));
		return parts.join(' · ');
	});
	function counts(value: AgentActivityCounts = {}) {
		const parts: string[] = [];
		if (value.results !== undefined) parts.push(tx(value.total !== undefined ? 'chat.agent.resultsTotal' : 'chat.agent.results', { count: fmt.format(value.results), total: value.total === undefined ? '' : fmt.format(value.total) }));
		else if (value.total !== undefined) parts.push(tx('chat.agent.total', { count: fmt.format(value.total) }));
		for (const key of ['segments', 'warnings', 'blocking', 'questions'] as const) {
			if (value[key] !== undefined) parts.push(tx(`chat.agent.counts.${key}`, { count: fmt.format(value[key]) }));
		}
		return parts.join(' · ');
	}
	/** A verification reports its result as a code; the lawyer reads words. */
	function detail(kind: ActivityObjectKind, value?: string) {
		return kind === 'verification' && value && ['passed', 'warnings', 'blocked'].includes(value)
			? $t(`chat.agent.verification.${value}`)
			: value;
	}
</script>

<section class="agent-activity min-w-0" aria-label={$t('chat.agent.activity')}>
	{#if !terminal}
		<div class="flex flex-wrap items-center gap-2.5 rounded-2xl border py-1.5 pl-1.5 pr-1.5 transition-colors duration-500 {waiting ? 'border-amber/25 bg-amber-soft/40 text-amber-soft-foreground' : 'border-border/70 bg-muted/30 text-foreground'}">
			<AgentOrb size={32} state={waiting ? 'breathing' : 'working'} tone={waiting ? 'amber' : 'brand'} />
			<!-- ThinkingRow's status includes its clock. Keep it out of the live region. -->
			<div aria-hidden="true" class="min-w-0 flex-1"><ThinkingRow {label} startedAt={run.created_at * 1000} /></div>
			<span class="sr-only" aria-live="polite" aria-atomic="true">{label}</span>
			{#if oncancel && !run.cancel_requested}
				<Button variant="ghost" size="sm" class="h-9 rounded-xl px-3 text-xs text-muted-foreground transition-colors hover:bg-destructive/10 hover:text-destructive focus-visible:ring-2 focus-visible:ring-brand ring-offset-background" disabled={cancelling} onclick={() => oncancel?.(run.id)}>
					<Icon name="stop-circle" class="h-4 w-4" />{$t('plan.cancel')}
				</Button>
			{/if}
		</div>
		{#if run.cancel_requested}<p class="mt-2 px-1 text-xs text-muted-foreground">{$t('plan.cancelRequested')}</p>{/if}
	{:else if rows.length}
		<Button variant="ghost" size="sm" class="group -ml-1 h-auto min-h-11 max-w-full gap-2 whitespace-normal rounded-xl px-2.5 text-left text-xs text-muted-foreground transition-colors hover:bg-muted/70 hover:text-foreground focus-visible:ring-2 focus-visible:ring-brand ring-offset-background" aria-expanded={expanded} aria-controls={regionId} onclick={() => (expanded = !expanded)}>
			<Icon name="history" class="h-4 w-4" />
			{$t(expanded ? 'chat.agent.hideActivity' : 'chat.agent.activity')}
			<span class="font-mono text-2xs tabular-nums text-muted-foreground/70">{summary}</span>
			<Icon name="alt-arrow-down" class="h-3.5 w-3.5 transition-transform duration-300 {expanded ? 'rotate-180' : ''}" />
		</Button>
	{/if}
	<ol id={regionId} class="relative space-y-5 {visible.length ? 'mt-3 pl-7 before:absolute before:bottom-3 before:left-[11px] before:top-3 before:w-px before:bg-border/70' : ''}">
		{#each visible as [dispatch, calls] (dispatch)}
			<li class="rise-in relative space-y-2">
				<span class="absolute -left-7 top-0.5 flex h-[23px] w-[23px] items-center justify-center rounded-full bg-background" aria-hidden="true">
					<span class="h-1.5 w-1.5 rounded-full bg-muted-foreground/40"></span>
				</span>
				{#each [...new Set(calls.map((row) => row.progress_note))] as note}
					<p class="whitespace-pre-wrap break-words pt-0.5 text-xs leading-relaxed text-muted-foreground">{note}</p>
				{/each}
				<ul class="space-y-1.5">
					{#each calls as row (row.call_id)}
						{@const retried = row.retried || rows.some((prior) => prior.dispatch < row.dispatch && prior.status === 'error' && prior.tool === row.tool && prior.object.label === row.object.label)}
						{@const meta = [detail(row.object.kind, row.object.detail), row.counts ? counts(row.counts) : ''].filter(Boolean)}
						<li class="flex items-start gap-3 rounded-xl bg-muted/40 px-3 py-2.5 text-ui transition-colors hover:bg-muted/60">
							<span class="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-background text-muted-foreground shadow-xs" aria-hidden="true">
								<Icon name={icons[row.object.kind] ?? 'document-text'} active class="h-3.5 w-3.5" />
							</span>
							<div class="min-w-0 flex-1 space-y-0.5 pt-1">
								<p class="break-words leading-snug text-foreground">{row.object.label}</p>
								{#if meta.length}<p class="break-words text-2xs text-muted-foreground">{meta.join(' · ')}</p>{/if}
								{#if row.status === 'empty' && row.tool.startsWith('search_')}<p class="text-2xs text-muted-foreground">{$t('chat.agent.emptySearch')}</p>{/if}
								{#if retried}<p class="text-2xs text-muted-foreground">{$t('chat.agent.retried')}</p>{/if}
							</div>
							<span class="pt-1.5" title={$t(`chat.agent.status.${row.status}`)}>
								<StepGlyph state={row.status === 'ok' ? 'succeeded' : row.status === 'empty' ? 'skipped' : row.status === 'error' ? 'failed' : 'running'} />
								<span class="sr-only">{$t(`chat.agent.status.${row.status}`)}</span>
							</span>
						</li>
					{/each}
				</ul>
			</li>
		{/each}
	</ol>
</section>
