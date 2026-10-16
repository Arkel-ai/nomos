<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	// Create a collection. The list mockup's create surface is a right-side
	// Sheet, sectioned form, Cancel ghost on
	// the left and Save on the right.
	import { t } from '$lib/i18n';
	import * as Sheet from '$lib/components/ui/sheet';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Button } from '$lib/components/ui/button';
	import { Separator } from '$lib/components/ui/separator';
	import { createDossier, matterTypes, type Dossier } from '$lib/api/dossiers';

	let {
		open = $bindable(false),
		oncreated
	}: { open?: boolean; oncreated?: (dossier: Dossier) => void } = $props();

	let title = $state('');
	let clientReference = $state('');
	let matterType = $state<string>('travail');
	let submitting = $state(false);
	let error = $state('');

	const valid = $derived(title.trim().length > 0);

	$effect(() => {
		if (open) return;
		title = '';
		clientReference = '';
		matterType = 'travail';
		error = '';
	});

	async function submit() {
		if (!valid || submitting) return;
		submitting = true;
		error = '';
		try {
			const dossier = await createDossier({
				title: title.trim(),
				client_reference: clientReference.trim(),
				matter_type: matterType
			});
			oncreated?.(dossier);
			open = false;
		} catch (err) {
			error = (err as { message?: string })?.message ?? $t('dossiers.createError');
		} finally {
			submitting = false;
		}
	}
</script>

<Sheet.Root bind:open>
	<Sheet.Content class="flex flex-col gap-4 sm:max-w-md">
		<Sheet.Header class="space-y-1">
			<Sheet.Title class="flex items-center gap-1.5 text-[13px] font-medium text-muted-foreground">
				<Icon name="add-circle" class="h-3.5 w-3.5 opacity-80" />
				{$t('dossiers.new')}
			</Sheet.Title>
			<Sheet.Description class="text-[11.5px] text-muted-foreground/80">
				{$t('dossiers.noneHint')}
			</Sheet.Description>
		</Sheet.Header>

		<div class="flex-1 space-y-4 overflow-y-auto px-4">
			<div class="space-y-3">
				<div class="text-[11px] font-medium uppercase tracking-wider text-muted-foreground/70">
					{$t('dossiers.overview')}
				</div>
				<div class="space-y-1.5">
					<Label for="dossier-title" class="text-[11.5px] text-muted-foreground">
						{$t('dossiers.name')}
					</Label>
					<Input
						id="dossier-title"
						bind:value={title}
						placeholder={$t('dossiers.namePlaceholder')}
					/>
				</div>
				<div class="space-y-1.5">
					<Label for="dossier-ref" class="text-[11.5px] text-muted-foreground">
						{$t('dossiers.clientReference')}
					</Label>
					<Input
						id="dossier-ref"
						bind:value={clientReference}
						placeholder={$t('dossiers.clientReferencePlaceholder')}
						class="font-mono"
					/>
				</div>
			</div>

			<Separator class="border-dashed" />

			<div class="space-y-3">
				<div class="text-[11px] font-medium uppercase tracking-wider text-muted-foreground/70">
					{$t('dossiers.matterType')}
				</div>
				<Select.Root type="single" bind:value={matterType}>
					<Select.Trigger class="text-[12.5px]">
						{$t(`dossiers.matterTypes.${matterType}`)}
					</Select.Trigger>
					<Select.Content>
						{#each matterTypes as type}
							<Select.Item value={type}>{$t(`dossiers.matterTypes.${type}`)}</Select.Item>
						{/each}
					</Select.Content>
				</Select.Root>
			</div>

			{#if error}
				<div class="text-[11.5px] text-destructive">{error}</div>
			{/if}
		</div>

		<Separator class="border-dashed" />

		<Sheet.Footer class="flex flex-row items-center justify-between gap-3 sm:justify-between">
			<Button variant="ghost" size="sm" onclick={() => (open = false)} disabled={submitting}>
				<Icon name="close-circle" class="mr-1 h-3.5 w-3.5" />
				{$t('common.cancel')}
			</Button>
			<Button size="sm" disabled={!valid || submitting} onclick={submit}>
				<Icon name="diskette" class="mr-1 h-3.5 w-3.5" />
				{submitting ? $t('common.saving') : $t('dossiers.create')}
			</Button>
		</Sheet.Footer>
	</Sheet.Content>
</Sheet.Root>
