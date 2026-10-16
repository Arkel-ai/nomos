<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// Export: one ZIP of renamed originals + one deterministic bordereau.docx.
	//
	// The POST is SYNCHRONOUS — the API renders through pandoc, revalidates
	// every citation, validates the OOXML that actually landed and publishes
	// both artifacts before it answers — so the response carries the two
	// published rows and there is nothing to follow. This card previously read
	// that response as a run and polled `getRun(undefined)`: it sat on
	// "export en cours" for ever and never offered the download, which is the
	// whole of "I cannot export the bordereau".
	//
	// Without an explicit approval both documents are stamped
	// "projet — vérification de l'avocat requise" (PLAN §13).
	import { t } from '$lib/i18n';
	import { tp } from './i18n-payload';
	import * as Card from '$lib/components/ui/card';
	import { Button } from '$lib/components/ui/button';
	import { Checkbox } from '$lib/components/ui/checkbox';
	import { Label } from '$lib/components/ui/label';
	import { sileo } from '$lib/components/ui/sileo';
	import { exportDossier } from '$lib/api/dossiers';
	import type { Artifact } from '$lib/api/runs';
	import ArtifactCard from '$lib/components/thread/ArtifactCard.svelte';

	let {
		dossierId,
		awaitingReview = 0
	}: { dossierId: string; awaitingReview?: number } = $props();

	let approve = $state(false);
	let running = $state(false);
	let artifacts = $state<Artifact[]>([]);
	let error = $state('');

	async function start() {
		if (running) return;
		running = true;
		error = '';
		try {
			const result = await exportDossier(dossierId, { approve });
			artifacts = result.artifacts ?? [];
			sileo.info({
				title: $t('dossiers.exportDone', tp({ count: result.entries?.length ?? 0 }))
			});
		} catch (err) {
			error = (err as { message?: string })?.message ?? $t('dossiers.exportError');
		} finally {
			running = false;
		}
	}
</script>

<Card.Root class="gap-4 rounded-2xl p-5">
	<div class="flex flex-wrap items-start justify-between gap-3">
		<div class="space-y-1">
			<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
				<Icon name="file-download" class="text-muted-foreground h-4 w-4" />
				{$t('dossiers.export')}
			</div>
			<p class="text-muted-foreground text-xs">{$t('dossiers.exportHint')}</p>
		</div>
		<Button class="h-9 px-4" disabled={running} onclick={start}>
			{#if running}
				<Spinner class="h-4 w-4" />
				{$t('dossiers.exportRunning')}
			{:else}
				<Icon name="file-download" class="h-4 w-4" />
				{$t('dossiers.export')}
			{/if}
		</Button>
	</div>

	<label
		for="export-approve"
		class="bg-muted/40 hover:bg-muted/70 flex cursor-pointer items-start gap-3 rounded-xl p-3.5 transition-colors"
	>
		<Checkbox
			id="export-approve"
			checked={approve}
			onCheckedChange={(v) => (approve = !!v)}
			aria-label={$t('dossiers.exportApprove')}
			class="mt-0.5"
		/>
		<span class="min-w-0 space-y-1">
			<span class="text-foreground block text-sm font-medium">{$t('dossiers.exportApprove')}</span>
			{#if !approve}
				<span
					class="text-amber inline-block -rotate-1 rounded border border-current px-1.5 py-px font-mono text-[10px] font-medium tracking-wide uppercase"
				>
					{$t('artifacts.draftBadge')}
				</span>
			{/if}
			{#if awaitingReview > 0}
				<span class="text-amber-soft-foreground flex items-center gap-1.5 text-xs">
					<Icon name="danger-triangle" class="h-3.5 w-3.5" />
					{$t('pieces.reviewNeeded', tp({ count: awaitingReview }))}
				</span>
			{/if}
		</span>
	</label>

	{#if error}
		<div class="text-destructive text-xs">{error}</div>
	{/if}

	<div class="space-y-2 pt-1">
		<div class="text-muted-foreground text-[10px] font-medium tracking-[0.1em] uppercase">
			{$t('artifacts.title')}
		</div>
		{#if !artifacts.length}
			<p class="text-muted-foreground text-xs">
				{running ? $t('dossiers.exportRunning') : $t('artifacts.none')}
			</p>
		{:else}
			<div class="space-y-2">
				{#each artifacts as artifact, i (artifact.id)}
					<div class="rise-in" style="animation-delay: {Math.min(i * 60, 240)}ms">
						<ArtifactCard {artifact} />
					</div>
				{/each}
			</div>
		{/if}
	</div>
</Card.Root>
