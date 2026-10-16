<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { t } from '$lib/i18n';
	import { Button } from '$lib/components/ui/button';
	import { isTerminalRunState, type StepState } from '$lib/api/runs';
	import type { RunView, StepView } from '$lib/stores/conversation';
	import StepGlyph from './StepGlyph.svelte';

	/**
	 * The plan assembling itself inside the turn (docs/PLAN.md §4.1, §11).
	 *
	 * Two things this component refuses to flatten:
	 *   - PARALLELISM. Steps are laid out by dependency depth, computed from the
	 *     `depends_on` edges the server sends — never guessed from arrival
	 *     order. A depth holding more than one step is drawn as a fan-out.
	 *   - RETRIES. An attempt count above the first is shown, so a lawyer can
	 *     see that the system recovered rather than silently repeating work.
	 */

	let {
		run,
		oncancel,
		cancelling = false
	}: {
		run: RunView;
		oncancel?: (runId: string) => void;
		cancelling?: boolean;
	} = $props();

	// sveltekit-i18n types the default payload as empty, so an interpolated
	// key (`{{count}}`) needs a widened signature — the same shim the
	// composer uses until the shared config declares one.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);


	const KNOWN_WORKFLOWS = new Set(['dossier_organize_v1']);

	const workflowLabel = $derived(
		KNOWN_WORKFLOWS.has(String(run.workflow_type))
			? $t(`plan.workflows.${run.workflow_type}`)
			: String(run.workflow_type || $t('plan.title'))
	);

	const done = $derived(run.steps.filter((s) => s.state === 'succeeded').length);
	const total = $derived(run.steps.length);
	const live = $derived(!isTerminalRunState(run.state));

	const runTone = $derived(
		run.state === 'failed'
			? 'text-destructive bg-destructive/10'
			: run.state === 'waiting_input'
				? 'text-amber-soft-foreground bg-amber-soft'
				: run.state === 'succeeded'
					? 'text-pos-soft-foreground bg-pos-soft'
					: run.state === 'cancelled'
						? 'text-muted-foreground bg-muted'
						: 'text-brand-soft-foreground bg-brand-soft'
	);

	const canCancel = $derived(!isTerminalRunState(run.state) && !run.cancel_requested);

	/**
	 * Dependency depth per step. Edges may name a step id or a step key, so both
	 * are resolvable; an edge pointing outside this plan is ignored rather than
	 * allowed to shift the layout.
	 */
	function levelsOf(steps: StepView[]): StepView[][] {
		const index = new Map<string, StepView>();
		for (const step of steps) {
			index.set(step.id, step);
			if (step.step_key) index.set(step.step_key, step);
		}

		const depth = new Map<string, number>();
		const visiting = new Set<string>();

		function depthOf(step: StepView): number {
			const cached = depth.get(step.id);
			if (cached !== undefined) return cached;
			// A cycle is a server bug, not a reason to hang the browser.
			if (visiting.has(step.id)) return 0;
			visiting.add(step.id);
			let value = 0;
			for (const edge of step.depends_on ?? []) {
				const parent = index.get(edge);
				if (!parent || parent.id === step.id) continue;
				value = Math.max(value, depthOf(parent) + 1);
			}
			visiting.delete(step.id);
			depth.set(step.id, value);
			return value;
		}

		const groups = new Map<number, StepView[]>();
		for (const step of steps) {
			const key = depthOf(step);
			const bucket = groups.get(key);
			if (bucket) bucket.push(step);
			else groups.set(key, [step]);
		}
		return [...groups.entries()].sort((a, b) => a[0] - b[0]).map(([, group]) => group);
	}

	const levels = $derived(levelsOf(run.steps));

	function stepLabel(step: StepView): string {
		return step.label || step.step_key || String(step.kind);
	}

	function stateLabel(state: StepState | string): string {
		return $t(`plan.state.${state}`);
	}
</script>

{#snippet stepRow(step: StepView, index: number)}
	<div
		class="rise-in flex items-start gap-3 rounded-xl px-2.5 py-2 transition-colors duration-300 {step.state ===
		'running'
			? 'bg-card shadow-sm'
			: step.state === 'waiting_input'
				? 'bg-amber-soft'
				: ''}"
		style="animation-delay: {Math.min(index * 45, 240)}ms"
	>
		<span class="mt-0.5">
			<StepGlyph state={step.state} />
		</span>
		<div class="min-w-0 flex-1">
			<div class="flex flex-wrap items-center gap-x-2 gap-y-1">
				<span
					class="text-sm leading-snug transition-colors duration-300 {step.state === 'succeeded'
						? 'text-muted-foreground'
						: step.state === 'queued'
							? 'text-muted-foreground/70'
							: 'text-foreground'}"
				>
					{stepLabel(step)}
				</span>

				<span
					class="border-border text-muted-foreground/80 rounded-md border px-1.5 py-px font-mono text-[10px] uppercase tracking-wide"
				>
					{$t(`plan.kind.${step.kind}`)}
				</span>

				{#if step.attempt > 1}
					<!-- The system retried and kept going. Say so. -->
					<span
						class="bg-amber-soft text-amber-soft-foreground rounded-md px-1.5 py-px text-2xs font-medium tabular-nums"
					>
						{tx('plan.attempt', { number: step.attempt, max: step.max_attempts })}
					</span>
				{/if}

				{#if step.state === 'waiting_input'}
					<span class="text-amber-soft-foreground text-2xs font-medium">
						{stateLabel(step.state)}
					</span>
				{/if}
			</div>

			{#if step.detail}
				<p class="text-muted-foreground mt-0.5 text-xs leading-snug">{step.detail}</p>
			{/if}

			{#if step.state === 'running' && step.progress !== undefined}
				<div class="bg-muted mt-2 h-[3px] w-full overflow-hidden rounded-full">
					<div
						class="from-brand to-signal h-full rounded-full bg-gradient-to-r transition-[width] duration-500"
						style="width: {Math.round(Math.min(Math.max(step.progress, 0), 1) * 100)}%"
					></div>
				</div>
			{/if}

			{#if step.state === 'failed'}
				<p class="text-destructive mt-0.5 text-xs leading-snug">
					{step.error_summary || $t('plan.failure')}
					{#if step.error_code}
						<span class="text-muted-foreground font-mono text-2xs"> · {step.error_code}</span>
					{/if}
				</p>
			{/if}
		</div>
	</div>
{/snippet}

<section
	class="border-border bg-muted/40 overflow-hidden rounded-2xl border"
	aria-label={$t('plan.title')}
>
	<header class="flex flex-wrap items-center gap-2 px-4 pt-3 pb-2.5">
		<span class="text-foreground text-ui font-semibold">{$t('plan.title')}</span>
		<span class="text-muted-foreground text-ui">· {workflowLabel}</span>

		<span
			class="ml-auto inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-2xs font-medium transition-colors duration-300 {runTone}"
		>
			{$t(`plan.runState.${run.state}`)}
		</span>

		{#if total > 0}
			<span class="text-muted-foreground font-mono text-2xs tabular-nums">
				{tx('plan.stepsProgress', { done, total })}
			</span>
		{/if}

		{#if canCancel && oncancel}
			<Button
				variant="ghost"
				size="sm"
				class="text-muted-foreground hover:text-destructive h-6 px-2 text-xs"
				disabled={cancelling}
				onclick={() => oncancel?.(run.id)}
			>
				<Icon name="stop-circle" class="h-3.5 w-3.5" />
				{$t('plan.cancel')}
			</Button>
		{/if}
	</header>

	{#if total > 0}
		<!-- The whole run at a glance: a hairline that fills as steps land. -->
		<div class="bg-border/70 mx-4 h-px overflow-hidden">
			<div
				class="h-full transition-[width] duration-700 {live
					? 'from-brand to-signal bg-gradient-to-r'
					: run.state === 'succeeded'
						? 'bg-pos'
						: 'bg-muted-foreground/50'}"
				style="width: {Math.round((done / total) * 100)}%"
			></div>
		</div>
	{/if}

	{#if run.cancel_requested && !isTerminalRunState(run.state)}
		<p class="text-muted-foreground px-4 pt-2.5 text-xs">
			{$t('plan.cancelRequested')}
		</p>
	{/if}

	<div class="space-y-1 p-2">
		{#if !run.planned && total === 0}
			<p class="shimmer px-2.5 py-2 text-sm">{$t('chat.assistantThinking')}</p>
		{/if}

		{#each levels as level, depth (depth)}
			{#if level.length > 1}
				<!-- A real fan-out: drawn as branches off one rail, never as a list. -->
				<div class="border-border relative rounded-xl border border-dashed p-1.5">
					<div
						class="text-muted-foreground flex items-center gap-1.5 px-2.5 pt-1 pb-1.5 text-2xs font-medium tracking-[0.08em] uppercase"
					>
						<Icon name="branching-paths-up" class="h-3 w-3" />
						{$t('plan.parallel')} · {level.length}
					</div>
					<div class="grid gap-1 sm:grid-cols-2">
						{#each level as step, i (step.id)}
							{@render stepRow(step, i)}
						{/each}
					</div>
				</div>
			{:else}
				{#each level as step (step.id)}
					{@render stepRow(step, depth)}
				{/each}
			{/if}
		{/each}
	</div>
</section>
