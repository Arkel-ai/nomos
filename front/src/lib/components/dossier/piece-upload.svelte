<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// Multi-file upload for one collection (PLAN §7.1).
	//
	// Files are posted one request per file so each line reports its own state
	// and, when the server refuses it, the reason the API actually returned
	// (encrypted PDF, wrong magic bytes, too large) — never a generic failure.
	import { t } from '$lib/i18n';
	import { tp } from './i18n-payload';
	import { uploadPieces, type RejectedUpload } from '$lib/api/pieces';
	import { FileDropZone, displaySize, MEGABYTE } from '$lib/components/ui/file-drop-zone';
	import { Progress } from '$lib/components/ui/progress';
	import { Button } from '$lib/components/ui/button';

	let {
		dossierId,
		onuploaded
	}: { dossierId: string; onuploaded?: (runId: string | null) => void } = $props();

	type EntryState = 'queued' | 'uploading' | 'done' | 'rejected';
	type Entry = { id: string; name: string; size: number; state: EntryState; reason: string };

	const MAX_FILE_SIZE = 50 * MEGABYTE;

	let entries = $state<Entry[]>([]);
	let busy = $state(false);

	const rejected = $derived(entries.filter((e) => e.state === 'rejected'));
	const progress = $derived.by(() => {
		if (!entries.length) return 0;
		const settled = entries.filter((e) => e.state === 'done' || e.state === 'rejected').length;
		return Math.round((settled / entries.length) * 100);
	});

	function set(id: string, patch: Partial<Entry>) {
		entries = entries.map((e) => (e.id === id ? { ...e, ...patch } : e));
	}

	/** The API reports each refusal by name; match it back onto its line. */
	function applyRejections(entry: Entry, list: RejectedUpload[] | undefined) {
		const mine = (list ?? []).find((r) => r.name === entry.name) ?? (list ?? [])[0];
		if (!mine) return false;
		set(entry.id, { state: 'rejected', reason: mine.reason });
		return true;
	}

	async function upload(files: File[]) {
		if (!files.length) return;
		const fresh: Entry[] = files.map((f) => ({
			id: crypto.randomUUID(),
			name: f.name,
			size: f.size,
			state: 'queued',
			reason: ''
		}));
		entries = [...entries, ...fresh];
		busy = true;
		let lastRun: string | null = null;

		for (let i = 0; i < files.length; i++) {
			const entry = fresh[i];
			set(entry.id, { state: 'uploading' });
			try {
				const res = await uploadPieces(dossierId, [files[i]]);
				if (res.accepted?.length) {
					set(entry.id, { state: 'done', reason: '' });
					lastRun = res.run_id ?? lastRun;
				} else if (!applyRejections(entry, res.rejected)) {
					set(entry.id, { state: 'rejected', reason: $t('pieces.uploadError') });
				}
			} catch (err) {
				set(entry.id, {
					state: 'rejected',
					reason: (err as { message?: string })?.message ?? $t('pieces.uploadError')
				});
			}
		}

		busy = false;
		onuploaded?.(lastRun);
	}

	/** The drop zone's own client-side refusals (size, type) get a line too. */
	function onFileRejected({ file, reason }: { file: File; reason: string }) {
		entries = [
			...entries,
			{
				id: crypto.randomUUID(),
				name: file.name,
				size: file.size,
				state: 'rejected',
				reason
			}
		];
	}
</script>

<div class="space-y-3">
	<FileDropZone
		accept="application/pdf,.pdf,application/vnd.openxmlformats-officedocument.wordprocessingml.document,.docx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet,.xlsx"
		maxFileSize={MAX_FILE_SIZE}
		disabled={busy}
		onUpload={upload}
		{onFileRejected}
		class="bg-muted/30 hover:bg-muted/60 hover:border-foreground/20 h-36 rounded-2xl border-[1.5px] border-border transition-colors"
	>
		<div class="flex flex-col items-center justify-center gap-2 text-center">
			<div
				class="bg-brand-soft text-brand flex h-10 w-10 items-center justify-center rounded-xl"
			>
				<Icon name="cloud-upload" active class="h-5 w-5" />
			</div>
			<div class="text-foreground text-sm font-medium">{$t('pieces.dropzone')}</div>
			<div class="text-muted-foreground text-xs">
				{$t('pieces.uploadHint')} · {displaySize(MAX_FILE_SIZE)}
			</div>
		</div>
	</FileDropZone>

	{#if entries.length}
		<div class="space-y-2">
			<div class="flex items-center justify-between gap-3">
				<div class="text-muted-foreground text-[10px] font-medium tracking-[0.1em] uppercase">
					{busy ? $t('pieces.uploading') : $t('pieces.accepted', tp({ count: entries.filter((e) => e.state === 'done').length }))}
				</div>
				{#if !busy}
					<Button
						variant="ghost"
						size="sm"
						class="text-muted-foreground h-7 text-xs"
						onclick={() => (entries = [])}
					>
						{$t('pieces.uploadDismiss')}
					</Button>
				{/if}
			</div>

			<Progress value={progress} class="h-1" />

			<div class="divide-border divide-y rounded-xl border">
				{#each entries as entry (entry.id)}
					<div class="rise-in flex items-center gap-3 px-3 py-2">
						<Icon name="file-text" class="text-muted-foreground h-4 w-4 shrink-0" />
						<div class="min-w-0 flex-1">
							<div class="truncate text-ui font-medium">{entry.name}</div>
							<div class="text-muted-foreground truncate text-2xs">
								{displaySize(entry.size)}
								{#if entry.state === 'rejected'}
									· <span class="text-destructive">{entry.reason}</span>
								{/if}
							</div>
						</div>
						<div class="shrink-0 text-xs">
							{#if entry.state === 'queued'}
								<span class="text-muted-foreground">{$t('pieces.uploadQueued')}</span>
							{:else if entry.state === 'uploading'}
								<Spinner class="h-3.5 w-3.5 text-muted-foreground" />
							{:else if entry.state === 'done'}
								<span class="inline-flex items-center gap-1 text-pos">
									<Icon name="check-circle" active class="h-3.5 w-3.5" />
									{$t('pieces.uploadDone')}
								</span>
							{:else}
								<span class="inline-flex items-center gap-1 text-destructive">
									<Icon name="close-circle" class="h-3.5 w-3.5" />
									{$t('common.error')}
								</span>
							{/if}
						</div>
					</div>
				{/each}
			</div>

			{#if rejected.length}
				<div class="border-destructive/30 bg-destructive/5 rounded-xl border p-3">
					<div class="text-destructive text-ui font-medium">
						{$t('pieces.uploadRejectedTitle')} · {rejected.length}
					</div>
					<div class="text-muted-foreground mt-0.5 text-xs">
						{$t('pieces.uploadRejectedHint')}
					</div>
					<ul class="mt-2 space-y-1">
						{#each rejected as r (r.id)}
							<li class="text-xs">
								<span class="font-medium">{r.name}</span>
								<span class="text-muted-foreground"> — {r.reason}</span>
							</li>
						{/each}
					</ul>
				</div>
			{/if}
		</div>
	{/if}
</div>
