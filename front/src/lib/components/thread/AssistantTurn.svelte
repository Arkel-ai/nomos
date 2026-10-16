<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import NomosMark from '$lib/components/nomos-mark.svelte';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { t, locale } from '$lib/i18n';
	import type { Message } from '$lib/api/conversations';
	import type { RunView } from '$lib/stores/conversation';
	import { isTerminalRunState, type QuestionAnswer } from '$lib/api/runs';
	import type { ThreadCitation } from './types';
	import PlanMonitor from './PlanMonitor.svelte';
	import ClarificationBlock from './ClarificationBlock.svelte';
	import ArtifactCard from './ArtifactCard.svelte';
	import CitationList from './CitationList.svelte';
	import StreamingText from './StreamingText.svelte';
	import ThinkingRow from './ThinkingRow.svelte';
	import AgentOrb from './AgentOrb.svelte';
	import { conversation } from '$lib/stores/conversation';
	import AgentActivity from './AgentActivity.svelte';
	import CoverageSummary from './CoverageSummary.svelte';
	import CitationReference from './CitationReference.svelte';
	import { runActivity } from './activity';

	/**
	 * AN ASSISTANT TURN IS A CONTAINER, not a paragraph (docs/PLAN.md §11).
	 *
	 * In order it may hold: the streamed conversational reply, the live plan
	 * monitor, the clarification cards, the artifacts, and the citations. Any
	 * of them may be absent — a plain answer is just the first block.
	 *
	 * The avatar is the machine's state: the orb while Nomos works (its figure
	 * follows the running step, amber while it waits on the lawyer), the
	 * still mark once the turn is settled.
	 *
	 * An agent run (`nomos_agent`) never shows the
	 * plan monitor — its runner steps are plumbing. It renders, in order: the
	 * verified answer, the activity, the outcome, lawyer questions, coverage,
	 * artifacts and sources; its orb lives in the activity summary.
	 */

	let {
		oninspect,
		loadCitation,
		coverageExpanded = false,
		message,
		run = null,
		citations = [],
		artifactsDownloadable = true,
		busy = false,
		onanswer,
		oncancel,
		cancelling = false
	}: {
		oninspect?: () => void;
		loadCitation?: typeof import('$lib/api/citations').getCitation;
		coverageExpanded?: boolean;
		message: Message;
		run?: RunView | null;
		citations?: ThreadCitation[];
		artifactsDownloadable?: boolean;
		busy?: boolean;
		onanswer?: (runId: string, answers: QuestionAnswer[]) => Promise<unknown> | void;
		oncancel?: (runId: string) => void;
		cancelling?: boolean;
	} = $props();

	// --- agent turns (workflow_type nomos_agent) ---------------------------
	const agent = $derived(run?.workflow_type === 'nomos_agent');
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);
	const agentCitations = $derived(run?.citations ?? []);
	/** This answer's sources, in the server's first-use order. */
	const references = $derived(
		agentCitations
			.filter((citation) => citation.message_id === message.id)
			.sort((a, b) => a.number - b.number)
	);
	/** Sources stay collapsed until asked for, or until a [n] reference points into them. */
	let sourcesOpen = $state(false);
	function revealSource() {
		sourcesOpen = true;
		oninspect?.();
	}
	const pendingPieces = $derived(
		run?.coverage?.pieces.filter((piece) => piece.state === 'pending').length
	);

	/**
	 * Terminal outcome → fixed copy and tone. Never optimistic: `completed`
	 * says nothing, and an unknown failure reads as a failure.
	 */
	const OUTCOMES: Record<string, { key: string; tone: 'muted' | 'destructive' }> = {
		partial: { key: 'chat.agent.partial', tone: 'muted' },
		cancelled: { key: 'plan.runState.cancelled', tone: 'muted' },
		agent_turn_limit: { key: 'chat.agent.limitReached', tone: 'muted' },
		agent_deadline: { key: 'chat.agent.limitReached', tone: 'muted' },
		verification_refused: { key: 'artifacts.verificationBlocked', tone: 'destructive' },
		agent_failed: { key: 'chat.agent.failed', tone: 'destructive' },
		no_progress: { key: 'chat.agent.failed', tone: 'destructive' },
		submission_required: { key: 'chat.agent.failed', tone: 'destructive' },
		invalid_tool_call: { key: 'chat.agent.failed', tone: 'destructive' },
		model_refusal: { key: 'chat.agent.failed', tone: 'destructive' },
		model_truncated: { key: 'chat.agent.failed', tone: 'destructive' }
	};
	const outcome = $derived.by(() => {
		if (!run || !isTerminalRunState(run.state)) return null;
		const code =
			[message.agent_outcome, run.failure_code].find((c) => c && c in OUTCOMES) ??
			(run.state === 'cancelled' ? 'cancelled' : run.state === 'failed' ? 'agent_failed' : null);
		return code ? { code, ...OUTCOMES[code] } : null;
	});
	const streaming = $derived(message.state === 'streaming');
	const reading = $derived(message.state === 'reading');
	const hasPlan = $derived(!!run && (run.planned || run.steps.length > 0));
	const questions = $derived(run?.questions ?? []);
	const artifacts = $derived(run?.artifacts ?? []);

	const activity = $derived(
		run && run.state !== 'succeeded' && run.state !== 'failed' && run.state !== 'cancelled'
			? runActivity(run)
			: streaming || reading
				? { state: message.content ? ('composing' as const) : ('working' as const), tone: 'brand' as const }
				: null
	);
</script>

<div class="grid grid-cols-[28px_minmax(0,1fr)] gap-3.5">
	<span class="flex h-7 w-7 items-center justify-center" aria-hidden="true">
		{#if activity && !agent}
			<AgentOrb state={activity.state} tone={activity.tone} size={32} />
		{:else}
			<NomosMark size={22} />
		{/if}
	</span>

	<div class="min-w-0 space-y-5 pt-0.5">
		{#if agent && run}
			<!-- 1. the verified answer: the server streams only stored, checked text -->
			{#if message.content}
				<StreamingText
					text={message.content}
					{streaming}
					class="text-foreground text-read [overflow-wrap:anywhere]"
				/>
				{#if references.length}
					<div class="flex flex-wrap items-center gap-1" role="group" aria-label={$t('citations.title')}>
						<span class="mr-1 text-2xs text-muted-foreground" aria-hidden="true">{$t('citations.references')}</span>
						{#each references as citation (citation.id)}
							<CitationReference oninspect={revealSource} id={citation.id} number={citation.number} />
						{/each}
					</div>
				{/if}
			{:else if reading}
				<p class="text-muted-foreground text-sm">
					{$t('chat.agent.readingDocuments')}
					{#if pendingPieces !== undefined}
						<span class="font-mono tabular-nums">
							{tx('chat.agent.pendingDocuments', {
								count: new Intl.NumberFormat($locale || 'fr').format(pendingPieces)
							})}
						</span>
					{/if}
				</p>
			{/if}

			<!-- 2. what the agent did: live, then collapsed -->
			<AgentActivity {run} {oncancel} {cancelling} />

			{#if outcome}
				<p
					class="rounded-xl p-3 text-ui {outcome.tone === 'destructive'
						? 'bg-destructive/10 text-destructive'
						: 'bg-muted text-muted-foreground'}"
				>
					{tx(outcome.key, { reference: run.id })}
				</p>
			{/if}

			<!-- 3. ask_lawyer, through the existing clarification flow -->
			{#if questions.length}
				<ClarificationBlock
					{questions}
					{busy}
					agent
					onstale={() => conversation.reloadRun(run.id)}
					onanswer={(answers) => onanswer?.(run.id, answers)}
				/>
			{/if}

			<!-- 4–6. the turn's cards, one quiet stack: coverage, deliverables, sources -->
			{#if (run.coverage && run.coverage.total_pieces > 0) || (outcome?.code !== 'verification_refused' && artifacts.length) || agentCitations.length || run.citations_error || (run.citations_loading && isTerminalRunState(run.state))}
				<div class="space-y-2.5">
					<!-- 4. which pièces were read -->
					{#if run.coverage && run.coverage.total_pieces > 0}
						<CoverageSummary coverage={run.coverage} expanded={coverageExpanded} />
					{/if}

					<!-- 5. published deliverables; a refused verification publishes none -->
					{#if outcome?.code !== 'verification_refused' && artifacts.length}
						<div class="space-y-2.5">
							{#each artifacts as artifact, i (artifact.id)}
								<div class="rise-in" style="animation-delay: {Math.min(i * 70, 240)}ms">
									<ArtifactCard {artifact} agent downloadable={artifactsDownloadable} />
								</div>
							{/each}
						</div>
					{/if}

					<!-- 6. sources, by type -->
					{#if run.citations_loading && !agentCitations.length && isTerminalRunState(run.state)}
						<div
							class="bg-card flex items-center gap-2.5 rounded-2xl border px-4 py-3"
							aria-busy="true"
							aria-label={$t('citations.loading')}
						>
							<Skeleton class="h-7 w-7 rounded-lg" />
							<Skeleton class="h-4 w-28" />
						</div>
					{:else if agentCitations.length || run.citations_error}
						<CitationList
							bind:expanded={sourcesOpen}
							total={run.citations_total}
							{oninspect}
							loadDetail={loadCitation}
							citations={agentCitations}
							nextAfter={run.citations_next_after}
							loading={run.citations_loading}
							error={run.citations_error}
							onmore={() => conversation.refreshAgentSources(run.id, true)}
							onretry={() => conversation.refreshAgentSources(run.id)}
						/>
					{/if}
				</div>
			{/if}
		{:else}
		<!-- a. the conversational reply, word by word -->
		{#if message.content}
			<StreamingText
				text={message.content}
				{streaming}
				class="text-foreground text-read"
			/>
		{:else if streaming || reading}
			<ThinkingRow
				label={$t(reading ? 'chat.assistantReading' : 'chat.assistantThinking')}
				startedAt={message.created_at ? message.created_at * 1000 : undefined}
			/>
		{/if}

		{#if message.state === 'failed'}
			<p class="text-destructive flex items-center gap-1.5 text-ui">
				<Icon name="danger-triangle" class="h-4 w-4" />
				{$t('chat.assistantFailed')}
			</p>
		{:else if message.truncated}
			<p class="text-muted-foreground flex items-center gap-1.5 text-ui">
				<Icon name="danger-triangle" class="h-4 w-4" />
				{$t('chat.assistantTruncated')}
			</p>
		{/if}

		<!-- b. the plan, assembling itself -->
		{#if run && hasPlan}
			<PlanMonitor {run} {oncancel} {cancelling} />
		{/if}

		<!-- c. clarifications, inline — never a modal over the thread -->
		{#if run && questions.length}
			<ClarificationBlock
				{questions}
				{busy}
				onanswer={(answers) => onanswer?.(run.id, answers)}
			/>
		{/if}

		<!-- d. deliverables -->
		{#if artifacts.length}
			<div class="space-y-2">
				{#each artifacts as artifact, i (artifact.id)}
					<div class="rise-in" style="animation-delay: {Math.min(i * 70, 240)}ms">
						<ArtifactCard {artifact} downloadable={artifactsDownloadable} />
					</div>
				{/each}
			</div>
		{/if}

		<!-- e. citations, in their three states -->
		{#if citations.length}
			<CitationList {citations} />
		{/if}
		{/if}
	</div>
</div>
