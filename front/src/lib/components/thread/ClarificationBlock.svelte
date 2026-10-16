<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { t } from '$lib/i18n';
	import { Button } from '$lib/components/ui/button';
	import Spinner from '$lib/components/spinner.svelte';
	import { Textarea } from '$lib/components/ui/textarea';
	import type { QuestionAnswer, QuestionOption, RunQuestion } from '$lib/api/runs';

	/**
	 * Clarification cards, INLINE IN THE TURN (docs/PLAN.md §4.4, §11).
	 *
	 * Never a modal: the questions that change the legal strategy are asked
	 * where the evidence is, and the lawyer keeps reading the thread while
	 * answering. A skip is recorded as an answer and reads as an explicit
	 * "Passée" afterwards — never as an unanswered question.
	 */

	let {
		agent = false,
		onstale,
		questions,
		onanswer,
		busy = false
	}: {
		agent?: boolean;
		onstale?: () => Promise<unknown>;
		questions: RunQuestion[];
		onanswer?: (answers: QuestionAnswer[]) => Promise<unknown> | void;
		busy?: boolean;
	} = $props();

	// sveltekit-i18n types the default payload as empty, so an interpolated
	// key (`{{count}}`) needs a widened signature — the same shim the
	// composer uses until the shared config declares one.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);


	let selections = $state<Record<string, string[]>>({});
	let texts = $state<Record<string, string>>({});
	let submitting = $state(false);
	let error = $state<string | null>(null);

	const ordered = $derived([...questions].sort((a, b) => a.asked_at - b.asked_at));
	const pending = $derived(ordered.filter((q) => q.state === 'pending'));
	const settled = $derived(ordered.length - pending.length);

	const hasSelection = $derived(
		pending.some((q) =>
			q.kind === 'text' ? (texts[q.question_key] ?? '').trim().length > 0 : (selections[q.question_key] ?? []).length > 0
		)
	);

	function toggle(question: RunQuestion, value: string) {
		const current = selections[question.question_key] ?? [];
		if (question.kind === 'multi') {
			selections[question.question_key] = current.includes(value)
				? current.filter((v) => v !== value)
				: [...current, value];
		} else {
			selections[question.question_key] = current.includes(value) ? [] : [value];
		}
	}

	function isSelected(question: RunQuestion, value: string): boolean {
		return (selections[question.question_key] ?? []).includes(value);
	}

	function answerFor(question: RunQuestion): QuestionAnswer | null {
		if (question.kind === 'text') {
			const text = (texts[question.question_key] ?? '').trim();
			return text ? { question_key: question.question_key, text } : null;
		}
		const values = selections[question.question_key] ?? [];
		return values.length ? { question_key: question.question_key, values } : null;
	}

	async function submit(answers: QuestionAnswer[]) {
		if (!answers.length || !onanswer) return;
		submitting = true;
		error = null;
		try {
			await onanswer(answers);
			for (const answer of answers) {
				delete selections[answer.question_key];
				delete texts[answer.question_key];
			}
		} catch (err) {
			// 409: the set was already settled or replaced. Show what the
			// server holds now rather than an error the lawyer cannot act on.
			error = 'clarification.submitError';
			if (agent && onstale && (err as { status?: number })?.status === 409) {
				try {
					await onstale();
					error = 'chat.agent.staleQuestion';
				} catch {
					/* the refetch failed too: keep the submit error */
				}
			}
		} finally {
			submitting = false;
		}
	}

	/** A skip is an answer (§4.4): it travels the same call, flagged. */
	function skip(question: RunQuestion) {
		void submit([{ question_key: question.question_key, skipped: true }]);
	}

	function skipAll() {
		void submit(pending.map((q) => ({ question_key: q.question_key, skipped: true })));
	}

	function submitAnswered() {
		void submit(pending.map(answerFor).filter((a): a is QuestionAnswer => a !== null));
	}

	function optionLabel(question: RunQuestion, value: string): string {
		return question.options?.find((o: QuestionOption) => o.value === value)?.label ?? value;
	}

	/** What the collapsed card says the lawyer chose. */
	function summaryOf(question: RunQuestion): string {
		const answer = question.answer as { values?: unknown; text?: unknown } | undefined;
		if (Array.isArray(answer?.values) && answer.values.length) {
			return answer.values.map((v) => optionLabel(question, String(v))).join(' · ');
		}
		if (typeof answer?.text === 'string' && answer.text.trim()) return answer.text.trim();
		return $t('clarification.answered');
	}

	const busyNow = $derived(busy || submitting);
</script>

<section
	class="rise-in bg-card overflow-hidden rounded-2xl border border-[color-mix(in_oklab,var(--amber)_35%,var(--border))] shadow-sm"
	aria-label={$t('clarification.title')}
>
	<header class="flex flex-wrap items-center gap-2 px-4 pt-3.5">
		<span class="bg-amber-soft text-amber flex h-6 w-6 items-center justify-center rounded-lg">
			<Icon name="question-circle" active class="h-3.5 w-3.5" />
		</span>
		<span class="text-foreground text-ui font-semibold">{$t('clarification.title')}</span>
		<span class="text-muted-foreground ml-auto font-mono text-2xs tabular-nums">
			{tx('clarification.progress', { current: settled, total: ordered.length })}
		</span>
	</header>

	<p class="text-muted-foreground px-4 pt-1.5 text-xs leading-snug">
		{$t('clarification.subtitle')}
	</p>

	<div class="space-y-2 p-3">
		{#each ordered as question (question.question_key)}
			{#if question.state === 'pending'}
				<div class="bg-muted/40 rounded-xl p-3.5">
					<div class="flex flex-wrap items-start gap-2">
						<p class="text-foreground flex-1 text-sm leading-snug font-medium">
							{question.prompt}
						</p>
						<span
							class="border-border text-muted-foreground/80 rounded-md border px-1.5 py-px font-mono text-[10px] tracking-wide uppercase"
						>
							{$t(`clarification.${question.kind}`)}
						</span>
					</div>

					{#if question.kind === 'text'}
						<Textarea
							bind:value={texts[question.question_key]}
							placeholder={$t('clarification.textPlaceholder')}
							rows={2}
							class="bg-card mt-2.5 text-sm"
							disabled={busyNow}
						/>
					{:else}
						<div class="mt-2.5 flex flex-wrap gap-1.5">
							{#each question.options ?? [] as option (option.value)}
								{@const active = isSelected(question, option.value)}
								<button
									type="button"
									disabled={busyNow}
									onclick={() => toggle(question, option.value)}
									title={option.hint}
									aria-pressed={active}
									class="flex cursor-pointer items-center gap-1.5 rounded-full border px-3 py-1.5 text-left text-ui transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-60 {active
										? 'bg-primary text-primary-foreground border-transparent shadow-sm'
										: 'border-border bg-card text-foreground hover:border-foreground/25'}"
								>
									{#if active}
										<Icon name="check-circle" active class="h-3.5 w-3.5" />
									{/if}
									<span class="min-w-0">
										{option.label}
										{#if option.hint}
											<span
												class="block text-2xs leading-tight {active
													? 'text-primary-foreground/70'
													: 'text-muted-foreground'}"
											>
												{option.hint}
											</span>
										{/if}
									</span>
								</button>
							{/each}
						</div>
					{/if}

					{#if agent && question.required}
						<p class="text-amber-soft-foreground mt-2.5 text-xs">{$t('chat.agent.requiredSkip')}</p>
					{/if}
					<div class="mt-2.5 flex items-center">
						<Button
							variant="ghost"
							size="sm"
							class="text-muted-foreground hover:text-foreground -ml-2 h-7 px-2 text-xs"
							disabled={busyNow}
							onclick={() => skip(question)}
						>
							<Icon name="skip-next" class="h-3.5 w-3.5" />
							{$t('clarification.skip')}
						</Button>
					</div>
				</div>
			{:else}
				<!-- Collapsed: what was chosen, or an explicit skip. -->
				<div class="flex flex-wrap items-center gap-2 rounded-xl px-3.5 py-2">
					<span class="text-muted-foreground min-w-0 flex-1 truncate text-ui">
						{question.prompt}
					</span>
					{#if question.state === 'skipped'}
						<span
							class="text-muted-foreground border-border inline-flex items-center gap-1 rounded-full border border-dashed px-2 py-0.5 text-2xs font-medium"
						>
							<Icon name="skip-next" class="h-3 w-3" />
							{$t('clarification.skipRecorded')}
						</span>
					{:else}
						<span
							class="bg-pos-soft text-pos-soft-foreground inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-2xs font-medium"
						>
							<Icon name="check-circle" active class="h-3 w-3" />
							{summaryOf(question)}
						</span>
					{/if}
				</div>
			{/if}
		{/each}
	</div>

	<!-- Outside the footer: after a refresh there may be nothing left to answer. -->
	{#if agent && error}
		<p
			class="px-4 pb-3 text-xs {error === 'chat.agent.staleQuestion'
				? 'text-muted-foreground'
				: 'text-destructive'}"
			role="status"
		>
			{$t(error)}
		</p>
	{/if}
	{#if pending.length}
		<footer class="border-border flex flex-wrap items-center gap-2 border-t px-4 py-3">
			{#if error && !agent}
				<span class="text-destructive text-xs">{$t(error)}</span>
			{/if}
			<Button
				variant="ghost"
				size="sm"
				class="text-muted-foreground hover:text-foreground ml-auto h-8 px-3 text-ui"
				disabled={busyNow}
				onclick={skipAll}
			>
				{$t('clarification.skipAll')}
			</Button>
			<Button
				size="sm"
				class="h-8 px-3.5 text-ui"
				disabled={busyNow || !hasSelection}
				onclick={submitAnswered}
			>
				{#if busyNow}<Spinner class="h-3.5 w-3.5" />{/if}
				{busyNow ? $t('clarification.submitting') : $t('clarification.submit')}
			</Button>
		</footer>
	{/if}
</section>
