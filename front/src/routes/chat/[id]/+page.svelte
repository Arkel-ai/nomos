<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { t } from '$lib/i18n';
	import { conversation } from '$lib/stores/conversation';
	import { collection, loadDossier, resetCurrentDossier, refreshPieces } from '$lib/stores/dossier';
	import {
		deleteConversation,
		intakeErrorKey,
		type MessageCreateRequest
	} from '$lib/api/conversations';
	import ConfirmDialog from '$lib/components/confirm-dialog.svelte';
	import { sileo } from '$lib/components/ui/sileo';
	import type { Attachment, ComposerPayload } from '$lib/components/composer/types';
	import { isTerminalRunState, type QuestionAnswer } from '$lib/api/runs';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { createDossier } from '$lib/api/dossiers';
	import Composer from '$lib/components/composer/Composer.svelte';
	import { fileStaged } from '$lib/components/composer/stage';
	import {
		AssistantTurn,
		UserTurn,
		CollectionPill,
		ConnectionBadge
	} from '$lib/components/thread';

	/**
	 * THE THREAD (docs/PLAN.md §4.4, §4.5, §4.5b, §11).
	 *
	 * A scrolling message column with the composer pinned at the bottom, the
	 * collection pill on top, and assistant turns that are containers: reply,
	 * plan monitor, clarification cards, artifacts, citations.
	 *
	 * Transport: the store subscribes over ONE durable cursor — SSE first,
	 * falling back to the cursor poll — and reduces both identically. On mount
	 * it reads the thread, takes the cursor that came back with it, and
	 * subscribes from there, so a reload replays only what was missed. A
	 * dropped connection is loss of observation, never a failed or cancelled
	 * run: it shows a quiet "reconnexion…" and nothing else changes.
	 * Cancelling is a separate, explicit button inside the plan monitor.
	 */

	const threadId = $derived($page.params.id);

	// ── Subscription ─────────────────────────────────────────────────────────
	$effect(() => {
		const id = threadId;
		if (!id) return;
		let stop: (() => void) | null = null;
		let dropped = false;

		void (async () => {
			const exists = await conversation.load(id);
			if (dropped) return;
			// A thread that is gone gets no composer: everything sent or
			// attached there would 404. Start a new one instead.
			if (!exists) {
				sileo.error({ title: $t('conversations.gone') });
				await goto('/chat', { replaceState: true });
				return;
			}
			// The cursor the GET returned is already in the store; connect()
			// resumes from it rather than replaying the whole log.
			stop = conversation.connect(id);
		})();

		return () => {
			dropped = true;
			stop?.();
		};
	});

	// ── The bound collection ─────────────────────────────────────────────────
	const boundDossierId = $derived($conversation.conversation?.dossier_id ?? null);

	$effect(() => {
		const dossierId = boundDossierId;
		if (dossierId) void loadDossier(dossierId);
		else resetCurrentDossier();
	});

	const collectionCount = $derived($collection?.count ?? 0);

	// ── Turns ────────────────────────────────────────────────────────────────
	const turns = $derived(
		$conversation.messages
			.filter((m) => m.role !== 'system')
			.map((message) => ({
				message,
				run: message.run_id
					? ($conversation.runs.find((r) => r.id === message.run_id) ?? null)
					: null
			}))
	);

	// ── Auto-scroll that never yanks the view ────────────────────────────────
	// Pinned to the bottom by default; a lawyer who has scrolled up to read is
	// left alone, and gets an explicit "jump to latest" instead.
	let scroller = $state<HTMLDivElement | null>(null);
	let pinned = $state(true);

	function onScroll() {
		if (!scroller) return;
		const distance = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight;
		pinned = distance < 120;
	}

	function scrollToLatest() {
		pinned = true;
		const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches;
		scroller?.scrollTo({ top: scroller.scrollHeight, behavior: reduce ? 'instant' : 'smooth' });
	}

	/** Everything that should move the view when it changes, as one value. */
	const liveSignature = $derived(
		[
			$conversation.messages.length,
			$conversation.messages.at(-1)?.content.length ?? 0,
			$conversation.runs
				.map((r) => `${r.state}:${r.steps.map((s) => s.state).join('')}:${r.questions.length}:${r.artifacts.length}`)
				.join('|')
		].join('#')
	);

	$effect(() => {
		liveSignature;
		const el = scroller;
		if (!pinned || !el) return;
		requestAnimationFrame(() => el.scrollTo({ top: el.scrollHeight }));
	});

	// The reply is revealed word by word after its tokens land, so the column
	// keeps growing between store updates: follow its height while pinned.
	let column = $state<HTMLDivElement | null>(null);
	$effect(() => {
		const el = column;
		if (!el) return;
		const observer = new ResizeObserver(() => {
			if (pinned && scroller) scroller.scrollTo({ top: scroller.scrollHeight });
		});
		observer.observe(el);
		return () => observer.disconnect();
	});

	/**
	 * One substantive agent turn per thread: while one runs — or waits on the
	 * lawyer — the server refuses another, so the composer says so up front.
	 */
	const activeAgentTurn = $derived(
		$conversation.runs.some(
			(run) => run.workflow_type === 'nomos_agent' && !isTerminalRunState(run.state)
		)
	);

	/** Nomos is working in this thread: the composer's border carries light. */
	const working = $derived(
		$conversation.sending ||
			$conversation.messages.some((m) => m.state === 'streaming' || m.state === 'reading') ||
			$conversation.runs.some((r) => !isTerminalRunState(r.state) && r.state !== 'waiting_input')
	);

	// ── Composer ─────────────────────────────────────────────────────────────
	let composerValue = $state('');
	let composerAttachments = $state<Attachment[]>([]);
	let composerSkill = $state<string | null>(null);
	let filing = $state(false);
	let answering = $state(false);
	let cancelling = $state(false);

	async function handleSubmit(payload: ComposerPayload) {
		const id = threadId;
		const content = (payload.text ?? '').trim();
		if (!id || !content || filing || $conversation.sending) return;
		if (activeAgentTurn) return;

		// Staged files become pièces of the bound collection first — or of one
		// created for them and bound now, which the API names from this turn.
		let attachments = payload.attachments;
		if (attachments.some((a) => a.file)) {
			filing = true;
			try {
				let dossierId = boundDossierId;
				if (!dossierId) {
					dossierId = (await createDossier({ title: $t('chat.collectionNew'), auto_title: true }))
						.id;
					// Bound before the upload: a retry files into this dossier, never a second one.
					await conversation.bind(dossierId);
				}
				const filed = await fileStaged(dossierId, attachments);
				// The pièces replace the staged files, so a retry re-sends the turn, not the uploads.
				composerAttachments = attachments = filed.attachments;
				if (filed.rejected) sileo.error({ title: $t('errors.unsupportedType') });
				void refreshPieces(dossierId);
			} catch (err) {
				sileo.error({ title: (err as { message?: string })?.message ?? $t('errors.uploadFailed') });
				return;
			} finally {
				filing = false;
			}
		}

		const body: MessageCreateRequest = {
			content,
			attachment_ids: attachments.map((a) => a.id),
			skill_slugs: payload.skill ? [payload.skill] : []
		};

		pinned = true;
		try {
			await conversation.send(id, body);
			composerValue = '';
			composerAttachments = [];
			composerSkill = null;
		} catch (err) {
			// The composer keeps the text; say why it did not go, or the
			// lawyer can only press Enter again.
			// A turn this view has not seen yet holds the thread: read it, so the
			// composer shows why it waits and frees itself when that turn ends.
			const runId = (err as { run_id?: string })?.run_id;
			if (runId) void conversation.reloadRun(runId).catch(() => {});
			const intakeKey = intakeErrorKey(err);
			sileo.error({
				title: intakeKey
					? $t(intakeKey)
					: ((err as { message?: string })?.message ?? $t('errors.saveFailed'))
			});
		}
	}

	let deleteOpen = $state(false);

	async function deleteThread() {
		const id = threadId;
		if (!id) return;
		await deleteConversation(id);
		sileo.success({ title: $t('conversations.deleted') });
		await goto('/chat');
	}

	async function handleAnswer(runId: string, answers: QuestionAnswer[]) {
		answering = true;
		try {
			await conversation.answer(runId, answers);
		} finally {
			answering = false;
		}
	}

	async function handleCancel(runId: string) {
		cancelling = true;
		try {
			await conversation.cancel(runId);
		} catch {
			/* the run state is re-read from the durable log either way */
		} finally {
			cancelling = false;
		}
	}
</script>

<svelte:head>
	<title>{$conversation.conversation?.title || $t('chat.newThread')} · {$t('title')}</title>
</svelte:head>

<div class="relative flex h-full min-h-0 flex-col">
	<!-- The title bar floats: messages scroll beneath it and fade out. -->
	<header class="pointer-events-none absolute inset-x-0 top-0 z-20">
		<div
			class="from-card via-card/90 pointer-events-auto flex h-16 items-center gap-3 bg-gradient-to-b to-transparent pr-14 pb-2 pl-5 md:px-5"
		>
			<div class="flex min-w-0 items-center gap-2.5">
				<h1 class="text-foreground max-w-[40ch] truncate text-sm font-medium">
					{$conversation.conversation?.title || $t('chat.newThread')}
				</h1>
				<span class="bg-border h-4 w-px shrink-0" aria-hidden="true"></span>
				<CollectionPill
					count={collectionCount}
					dossierId={boundDossierId}
					title={$collection?.title ?? ''}
				/>
			</div>
			<div class="ml-auto flex items-center gap-1">
				<!-- An agent turn keeps working server-side: say so, quietly. -->
				{#if activeAgentTurn && $conversation.reconnecting}
					<span class="text-muted-foreground text-xs" role="status">
						{$t('chat.agent.reconnecting')}
					</span>
				{/if}
				<ConnectionBadge
					reconnecting={$conversation.reconnecting && !activeAgentTurn}
					transport={$conversation.transport}
				/>
				{#if $conversation.conversation}
					<button
						type="button"
						onclick={() => (deleteOpen = true)}
						aria-label={$t('conversations.delete')}
						title={$t('conversations.delete')}
						class="text-muted-foreground hover:text-destructive hover:bg-destructive/10 flex h-8 w-8 cursor-pointer items-center justify-center rounded-lg transition-colors"
					>
						<Icon name="trash-bin-trash" class="h-4 w-4" />
					</button>
				{/if}
			</div>
		</div>
	</header>

	<!-- The message column. -->
	<div bind:this={scroller} onscroll={onScroll} class="min-h-0 flex-1 overflow-y-auto">
		<div bind:this={column} class="mx-auto w-full max-w-3xl space-y-8 px-5 pt-20 pb-10">
			{#if $conversation.loading && !turns.length}
				<!-- Shaped like the turns that are coming. -->
				<div class="space-y-8" aria-busy="true">
					<div class="flex justify-end">
						<Skeleton class="h-10 w-[46%] rounded-[20px]" />
					</div>
					<div class="grid grid-cols-[28px_minmax(0,1fr)] gap-3.5">
						<Skeleton class="h-7 w-7 rounded-full" />
						<div class="space-y-2.5 pt-1">
							<Skeleton class="h-3.5 w-[92%]" />
							<Skeleton class="h-3.5 w-[84%]" />
							<Skeleton class="h-3.5 w-[58%]" />
							<Skeleton class="mt-4 h-28 w-full rounded-2xl" />
						</div>
					</div>
				</div>
			{:else if $conversation.error && !turns.length}
				<div class="flex flex-col items-center gap-3 py-16 text-center">
					<span
						class="bg-destructive/10 text-destructive flex h-10 w-10 items-center justify-center rounded-xl"
					>
						<Icon name="danger-triangle" active class="h-5 w-5" />
					</span>
					<p class="text-muted-foreground text-sm">{$t($conversation.error)}</p>
				</div>
			{:else if !turns.length}
				<p class="text-muted-foreground py-16 text-center text-sm">
					{$t('chat.emptyThread')}
				</p>
			{/if}

			{#each turns as turn (turn.message.id)}
				{#if turn.message.role === 'user'}
					<UserTurn message={turn.message} />
				{:else}
					<AssistantTurn
						message={turn.message}
						run={turn.run}
						busy={answering}
						{cancelling}
						oninspect={() => (pinned = false)}
						onanswer={handleAnswer}
						oncancel={handleCancel}
					/>
				{/if}
			{/each}
		</div>
	</div>

	<!-- The composer, pinned; the column fades out behind it. -->
	<div class="relative px-5 pb-4">
		<div
			class="from-card pointer-events-none absolute inset-x-0 -top-10 h-10 bg-gradient-to-t to-transparent"
			aria-hidden="true"
		></div>
		{#if !pinned && turns.length}
			<div class="pointer-events-none absolute -top-12 right-0 left-0 z-10 flex justify-center">
				<button
					type="button"
					onclick={scrollToLatest}
					class="bg-card text-foreground pointer-events-auto inline-flex h-8 cursor-pointer items-center gap-1.5 rounded-full border px-3 text-xs font-medium shadow-md transition-transform hover:-translate-y-px"
				>
					<Icon name="arrow-down" class="h-3.5 w-3.5" />
					{$t('chat.jumpToLatest')}
				</button>
			</div>
		{/if}

		<div class="relative mx-auto w-full max-w-3xl">
			<Composer
				activeTurn={activeAgentTurn}
				bind:value={composerValue}
				bind:attachments={composerAttachments}
				bind:skill={composerSkill}
				collectionTitle={$collection?.title ?? null}
				placeholder={$t('chat.agent.placeholder')}
				busy={filing || $conversation.sending}
				live={working || filing}
				autofocus
				onsubmit={handleSubmit}
			/>
		</div>
	</div>
</div>

<ConfirmDialog
	bind:open={deleteOpen}
	title={$t('conversations.deleteTitle')}
	description={$t('conversations.deleteBody')}
	details={[$t('conversations.deleteMessages'), $t('conversations.deleteKeepsRuns')]}
	confirmLabel={$t('conversations.delete')}
	onconfirm={deleteThread}
/>
