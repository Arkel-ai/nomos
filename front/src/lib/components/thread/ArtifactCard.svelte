<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { t, locale } from '$lib/i18n';
	import { Button } from '$lib/components/ui/button';
	import Spinner from '$lib/components/spinner.svelte';
	import { downloadArtifact, type Artifact } from '$lib/api/runs';

	/**
	 * A deliverable inside the turn (docs/PLAN.md §11, §13).
	 *
	 * Anything the lawyer has not approved is stamped
	 * "projet — vérification de l'avocat requise". That badge is not decoration:
	 * an unapproved export must never read as filed-quality work.
	 */

	let {
		artifact,
		agent = false,
		downloadable = true
	}: { artifact: Artifact; downloadable?: boolean; agent?: boolean } = $props();

	// sveltekit-i18n types the default payload as empty, so an interpolated
	// key (`{{count}}`) needs a widened signature — the same shim the
	// composer uses until the shared config declares one.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);


	const KNOWN_KINDS = new Set(['bordereau_docx', 'export_zip', 'note_docx']);

	const isZip = $derived(String(artifact.kind) === 'export_zip');
	/** An agent deliverable reads by its title; the format moves to the meta line. */
	const extension = $derived(/\.([a-z0-9]{2,5})$/i.exec(artifact.name)?.[1] ?? '');
	const title = $derived(agent && extension ? artifact.name.slice(0, -extension.length - 1) : artifact.name);

	const kindLabel = $derived(
		KNOWN_KINDS.has(String(artifact.kind))
			? $t(`artifacts.kinds.${artifact.kind}`)
			: String(artifact.kind)
	);

	let downloading = $state(false);
	let error = $state<string | null>(null);

	async function download() {
		if (!downloadable) return;
		downloading = true;
		error = null;
		try {
			await downloadArtifact(artifact.id);
		} catch {
			error = 'artifacts.downloadError';
		} finally {
			downloading = false;
		}
	}
</script>

<article
	class={agent ? "bg-card grid grid-cols-[40px_minmax(0,1fr)] items-center gap-3.5 rounded-2xl border border-border p-4 transition-shadow duration-300 hover:shadow-sm sm:grid-cols-[40px_minmax(0,1fr)_auto]" : "bg-card hover:border-foreground/15 group flex items-center gap-3.5 rounded-2xl border p-3.5 shadow-xs transition-colors"}
>
	<span
		class="bg-brand-soft text-brand flex h-10 w-10 shrink-0 items-center justify-center rounded-xl"
	>
		<Icon name={isZip ? 'archive' : 'document-text'} active class="h-5 w-5" />
	</span>

	<div class="min-w-0 flex-1">
		<p title={artifact.name} class="text-foreground truncate text-sm font-medium">{title}</p>
		<div class="mt-1.5 flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
			{#if agent}
				<!-- The template id is plumbing; the lawyer sees the format. -->
				{#if extension}<span class="text-muted-foreground font-mono text-2xs uppercase tracking-wide">{extension}</span>{/if}
			{:else}
				<span class="text-muted-foreground font-mono text-2xs uppercase tracking-wide">
					{kindLabel}
					{#if artifact.template_version}
						· {$t('artifacts.template')} {artifact.template_version}
					{/if}
				</span>
			{/if}
			{#if artifact.char_count}
				<span class="text-muted-foreground/70 text-2xs tabular-nums">
					{tx('artifacts.charCount', { count: agent ? new Intl.NumberFormat($locale || 'fr').format(artifact.char_count) : artifact.char_count })}
				</span>
			{/if}
			{#if artifact.approved}
				<span
					class="bg-pos-soft text-pos-soft-foreground inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-2xs font-medium"
				>
					<Icon name="shield-check" active class="h-3 w-3" />
					{$t('artifacts.approvedBadge')}
				</span>
			{:else}
				<!-- Not decoration: an unapproved export never reads as filed work. -->
				<span
					class="bg-amber-soft text-amber-soft-foreground inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-2xs font-medium"
				>
					<span class="h-1.5 w-1.5 rounded-full bg-current" aria-hidden="true"></span>
					{$t('artifacts.draftBadge')}
				</span>
			{/if}
		</div>

		{#if agent && artifact.partial}<p class="mt-2 text-xs text-muted-foreground">{$t('artifacts.partial')}</p>{/if}
		{#if agent && (artifact.piece_warnings ?? 0) > 0}<p class="mt-2 w-fit rounded-lg bg-amber-soft px-2 py-1 text-2xs text-amber-soft-foreground">{tx('artifacts.pieceWarnings', { count: new Intl.NumberFormat($locale || 'fr').format(artifact.piece_warnings ?? 0) })}</p>{/if}
		{#if error}
			<p class="text-destructive mt-1.5 text-xs">{$t(error)}</p>
		{/if}
	</div>

	{#if downloadable}
		<Button
			variant={agent ? 'secondary' : 'outline'}
			size="sm"
			class={agent ? "col-span-2 h-10 w-full rounded-xl px-4 text-ui transition-colors sm:col-span-1 sm:w-auto focus-visible:ring-2 focus-visible:ring-brand ring-offset-background" : "h-8 shrink-0 px-3 text-ui"}
			disabled={downloading}
			onclick={download}
		>
			{#if downloading}
				<Spinner class="h-3.5 w-3.5" />
			{:else}
				<Icon name="download" class="h-3.5 w-3.5" />
			{/if}
			{$t(agent && error ? 'artifacts.retryDownload' : 'artifacts.download')}
		</Button>
	{/if}
</article>
