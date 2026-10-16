<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// =====================================================================
	// /dossiers/[id] — one collection: the slice-1 workhorse (PLAN §1, §11).
	// An editorial header (title, identity line, stats on hairlines), then
	// the working cards: pièces in and conversations side by side, the
	// classement full width, the export last. The pièces table keeps the
	// data-table styling.
	// =====================================================================
	import { onDestroy } from 'svelte';
	import { reveal } from '$lib/motion';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { t } from '$lib/i18n';
	import { tp } from '$lib/components/dossier/i18n-payload';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { sileo } from '$lib/components/ui/sileo';
	import EmptyState from '$lib/components/empty-state.svelte';
	import PieceUpload from '$lib/components/dossier/piece-upload.svelte';
	import PiecesTable from '$lib/components/dossier/pieces-table.svelte';
	import ExportCard from '$lib/components/dossier/export-card.svelte';
	import DossierConversations from '$lib/components/dossier/dossier-conversations.svelte';
	import DossierStateChip from '$lib/components/dossier/dossier-state-chip.svelte';
	import {
		currentDossier,
		loadDossier,
		refreshPieces,
		piecesAwaitingReview,
		piecesAnalysing,
		watchQualification,
		resetCurrentDossier
	} from '$lib/stores/dossier';
	import { createConversation } from '$lib/api/conversations';
	import { matterTypes, deleteDossier } from '$lib/api/dossiers';
	import ConfirmDialog from '$lib/components/confirm-dialog.svelte';
	import type { SolarIconName } from '$lib/icons/solar';

	const id = $derived(page.params.id ?? '');
	const detail = $derived($currentDossier.dossier);
	const loading = $derived($currentDossier.loading);
	const pieces = $derived($currentDossier.pieces);
	const awaiting = $derived($piecesAwaitingReview.length);
	// Qualification continues in the background after the upload returns; the
	// store polls while these are non-empty, so the count is live.
	const analysing = $derived($piecesAnalysing.length);

	let starting = $state(false);

	// The route component is reused across `/dossiers/a` → `/dossiers/b`, so the
	// load tracks the param rather than firing once on mount.
	$effect(() => {
		if (id) loadDossier(id);
	});

	onDestroy(resetCurrentDossier);

	const stats = $derived<{ label: string; value: number; icon: SolarIconName }[]>([
		{ label: $t('pieces.title'), value: detail?.piece_count ?? pieces.length, icon: 'documents' },
		{ label: $t('pieces.state.approved'), value: detail?.approved_count ?? 0, icon: 'check-circle' },
		{ label: $t('plan.title'), value: detail?.run_count ?? 0, icon: 'routing-2' }
	]);

	function matterLabel(value: string) {
		return (matterTypes as readonly string[]).includes(value)
			? $t(`dossiers.matterTypes.${value}`)
			: value;
	}

	function formatDate(seconds: number) {
		return new Date(seconds * 1000).toLocaleDateString();
	}

	let deleteOpen = $state(false);

	async function purgeDossier() {
		if (!detail) return;
		const purge = await deleteDossier(detail.id);
		if (purge.complete) sileo.success({ title: $t('dossiers.deleted') });
		else sileo.warning({ title: $t('dossiers.purgeIncomplete') });
		await goto('/dossiers');
	}

	async function startConversation() {
		if (starting || !detail) return;
		starting = true;
		try {
			const thread = await createConversation({ dossier_id: detail.id, title: detail.title });
			await goto(`/chat/${thread.id}`);
		} catch (err) {
			sileo.error({
				title: (err as { message?: string })?.message ?? $t('dossiers.startConversationError')
			});
			starting = false;
		}
	}
</script>

<div use:reveal class="mx-auto w-full max-w-6xl space-y-6 pb-6">
	<a
		href="/dossiers"
		class="text-muted-foreground hover:text-foreground inline-flex items-center gap-1.5 text-xs transition-colors"
	>
		<Icon name="arrow-left" class="h-3.5 w-3.5" />
		{$t('dossiers.back')}
	</a>

	{#if loading && !detail}
		<div class="space-y-4">
			<Skeleton class="h-3 w-48" />
			<Skeleton class="h-9 w-[420px] max-w-full" />
			<Skeleton class="h-20 w-full rounded-2xl" />
			<Skeleton class="h-64 w-full rounded-2xl" />
		</div>
	{:else if !detail}
		<Card.Root class="gap-4 rounded-2xl p-5">
			<EmptyState
				icon="folder"
				title={$currentDossier.error ? $t('dossiers.loadError') : $t('dossiers.notFound')}
			>
				<Button variant="ghost" size="sm" onclick={() => goto('/dossiers')}>
					{$t('dossiers.back')}
				</Button>
			</EmptyState>
		</Card.Root>
	{:else}
		<!-- ===== Header =============================================== -->
		<header class="flex flex-wrap items-end justify-between gap-x-6 gap-y-4">
			<div class="min-w-0 space-y-2.5">
				<div class="text-muted-foreground flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
					<span class="text-foreground/80 font-mono">{detail.client_reference || '—'}</span>
					<span class="bg-border h-3 w-px" aria-hidden="true"></span>
					<span>{matterLabel(detail.matter_type)}</span>
					<span class="bg-border h-3 w-px" aria-hidden="true"></span>
					<span>{$t('dossiers.updatedAt')} {formatDate(detail.updated_at)}</span>
					<DossierStateChip state={detail.state} />
				</div>
				<h1 class="display text-foreground text-[34px] sm:text-[40px]">{detail.title}</h1>
				<p class="text-muted-foreground max-w-xl text-sm">{$t('dossiers.startConversationHint')}</p>
			</div>
			<div class="flex items-center gap-2">
				<Button
					variant="ghost"
					class="text-muted-foreground hover:text-destructive hover:bg-destructive/10 h-9 px-3"
					onclick={() => (deleteOpen = true)}
				>
					<Icon name="trash-bin-trash" class="h-4 w-4" />
					{$t('common.delete')}
				</Button>
				<Button class="h-9 px-4" disabled={starting} onclick={startConversation}>
					{#if starting}
						<Spinner class="h-4 w-4" />
					{:else}
						<Icon name="chat-round-dots" class="h-4 w-4" />
					{/if}
					{$t('dossiers.startConversation')}
				</Button>
			</div>
		</header>

		<!-- ===== Stats, on hairlines =================================== -->
		<div class="bg-card grid grid-cols-1 divide-y rounded-2xl border sm:grid-cols-3 sm:divide-x sm:divide-y-0">
			{#each stats as stat (stat.label)}
				<div class="flex items-center gap-3.5 px-5 py-4">
					<Icon name={stat.icon} class="text-muted-foreground h-5 w-5 shrink-0" />
					<div class="min-w-0">
						<div class="text-muted-foreground text-[10px] font-medium tracking-[0.1em] uppercase">
							{stat.label}
						</div>
						<div class="text-foreground text-2xl leading-tight font-medium tabular-nums">
							{stat.value}
						</div>
					</div>
				</div>
			{/each}
		</div>

		<!-- ===== Pièces in · conversations ============================== -->
		<div class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,380px)]">
			<Card.Root class="gap-3 rounded-2xl p-5">
				<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
					<Icon name="upload" class="text-muted-foreground h-4 w-4" />
					{$t('pieces.upload')}
				</div>
				<PieceUpload
					dossierId={detail.id}
					onuploaded={() => refreshPieces(detail.id).then(() => watchQualification(detail.id))}
				/>
			</Card.Root>
			<DossierConversations dossierId={detail.id} dossierTitle={detail.title} />
		</div>

		<!-- ===== Classement ============================================ -->
		<Card.Root class="gap-4 rounded-2xl p-5">
			<div class="flex flex-wrap items-start justify-between gap-3">
				<div class="space-y-1">
					<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
						<Icon name="documents" class="text-muted-foreground h-4 w-4" />
						{$t('pieces.classement')}
						<span class="text-muted-foreground font-mono text-2xs font-normal tabular-nums">
							{$currentDossier.pieceTotal || pieces.length}
						</span>
					</div>
					<p class="text-muted-foreground text-xs">
						{analysing > 0 ? $t('pieces.analysingHint') : $t('pieces.classementHint')}
					</p>
				</div>
				<div class="flex items-center gap-2">
					{#if analysing > 0}
						<span
							class="bg-brand-soft text-brand-soft-foreground inline-flex h-6 items-center gap-1.5 rounded-full px-2.5 text-2xs font-medium"
						>
							<Spinner class="h-3 w-3" />
							{$t(
								'pieces.analysing',
								tp({ done: pieces.length - analysing, total: pieces.length })
							)}
						</span>
					{/if}
					{#if awaiting > 0}
						<span
							class="bg-amber-soft text-amber-soft-foreground inline-flex h-6 items-center rounded-full px-2.5 text-2xs font-medium"
						>
							{$t('pieces.reviewNeeded', tp({ count: awaiting }))}
						</span>
					{/if}
				</div>
			</div>
			{#if $currentDossier.error}
				<div class="text-destructive text-xs">{$currentDossier.error}</div>
			{/if}
			<PiecesTable {pieces} loading={loading && !pieces.length} />
		</Card.Root>

		<!-- ===== Export =============================================== -->
		<ExportCard dossierId={detail.id} awaitingReview={awaiting} />
	{/if}
</div>

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
	onconfirm={purgeDossier}
/>
