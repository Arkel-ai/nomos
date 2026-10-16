<script lang="ts">
	import { t } from '$lib/i18n';
	import type { DossierState } from '$lib/api/dossiers';

	// A dossier's state as a soft chip with a dot: active is calm green,
	// archived recedes, a scheduled purge is amber — never an ink block.
	let { state: dossierState }: { state: DossierState | string } = $props();

	const TONE: Record<string, string> = {
		active: 'bg-pos-soft text-pos-soft-foreground [--dot:var(--pos)]',
		archived: 'bg-muted text-muted-foreground [--dot:var(--muted-foreground)]',
		purge_pending: 'bg-amber-soft text-amber-soft-foreground [--dot:var(--amber)]'
	};
</script>

<span
	class="inline-flex h-5 items-center gap-1.5 rounded-full px-2 text-2xs font-medium {TONE[dossierState] ??
		TONE.archived}"
>
	<span class="h-1.5 w-1.5 rounded-full bg-[var(--dot)]" aria-hidden="true"></span>
	{$t(`dossiers.state.${dossierState}`)}
</span>
