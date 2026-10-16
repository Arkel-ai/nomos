<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	import * as AlertDialog from '$lib/components/ui/alert-dialog';
	import { Button } from '$lib/components/ui/button';
	import { t } from '$lib/i18n';

	/**
	 * The one confirmation for irreversible actions (deleting a thread, a
	 * dossier, clearing data). Says exactly what goes, keeps the dialog open
	 * while the action runs, and shows a failure in place instead of closing.
	 */
	let {
		open = $bindable(false),
		title,
		description,
		details = [],
		confirmLabel,
		onconfirm
	}: {
		open?: boolean;
		title: string;
		description: string;
		/** What the action takes with it, one line each. */
		details?: string[];
		confirmLabel: string;
		onconfirm: () => Promise<void>;
	} = $props();

	let working = $state(false);
	let error = $state('');

	$effect(() => {
		if (open) error = '';
	});

	async function confirm() {
		if (working) return;
		working = true;
		error = '';
		try {
			await onconfirm();
			open = false;
		} catch (err) {
			const api = err as { status?: number; message?: string };
			error = api?.status && api.message ? api.message : $t('common.error');
		} finally {
			working = false;
		}
	}
</script>

<AlertDialog.Root bind:open>
	<AlertDialog.Content class="max-w-md rounded-2xl">
		<AlertDialog.Header class="gap-3">
			<span
				class="bg-destructive/10 text-destructive flex h-10 w-10 items-center justify-center rounded-xl"
			>
				<Icon name="trash-bin-trash" active class="h-5 w-5" />
			</span>
			<AlertDialog.Title class="font-serif text-[24px] leading-tight font-normal tracking-tight">
				{title}
			</AlertDialog.Title>
			<AlertDialog.Description class="text-muted-foreground text-sm leading-relaxed">
				{description}
			</AlertDialog.Description>
		</AlertDialog.Header>

		{#if details.length}
			<ul class="bg-muted/50 space-y-1.5 rounded-xl p-3.5">
				{#each details as line}
					<li class="text-foreground flex items-start gap-2 text-ui">
						<span class="bg-destructive mt-[7px] h-1.5 w-1.5 shrink-0 rounded-full" aria-hidden="true"></span>
						{line}
					</li>
				{/each}
			</ul>
		{/if}

		{#if error}
			<p class="text-destructive text-ui">{error}</p>
		{/if}

		<AlertDialog.Footer>
			<AlertDialog.Cancel disabled={working}>{$t('common.cancel')}</AlertDialog.Cancel>
			<Button variant="destructive" disabled={working} onclick={confirm}>
				{#if working}<Spinner class="h-4 w-4" />{/if}
				{confirmLabel}
			</Button>
		</AlertDialog.Footer>
	</AlertDialog.Content>
</AlertDialog.Root>
