<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// =====================================================================
	// /skills — the compétences library (docs/PLAN.md §8, §11).
	//
	// The list-page layout: one Card.Root, icon + title + inline
	// count on the left, the primary action on the right, the canonical
	// empty state, a right-side Sheet for the create flow.
	//
	// The page exists to make ONE property visible: a compétence is an
	// IMMUTABLE VERSION. There is no "edit this version" affordance anywhere
	// — publishing a change appends version N+1 and the history keeps every
	// earlier definition, because reproducing an earlier pleading is the
	// whole point of the split between `skills` and `skill_versions`.
	// =====================================================================

	import { onMount } from 'svelte';
	import { fly } from 'svelte/transition';
	import { cubicOut } from 'svelte/easing';
	import * as Sheet from '$lib/components/ui/sheet';
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Select from '$lib/components/ui/select';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { Badge } from '$lib/components/ui/badge';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Button } from '$lib/components/ui/button';
	import { Textarea } from '$lib/components/ui/textarea';
	import { Separator } from '$lib/components/ui/separator';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import EmptyState from '$lib/components/empty-state.svelte';
	import { t } from '$lib/i18n';
	import { tp } from '$lib/components/dossier/i18n-payload';
	import {
		listSkills,
		createSkill,
		getSkill,
		deleteSkill,
		type Skill,
		type SkillDetail,
		type SkillVersion
	} from '$lib/api/skills';
	import { dossiers, loadDossiers } from '$lib/stores/dossier';
	import { listPieces, type Piece } from '$lib/api/pieces';

	// ---- state ----------------------------------------------------------
	let loaded = $state(false);
	let skills = $state<Skill[]>([]);
	let error = $state('');

	let createOpen = $state(false);
	let submitting = $state(false);
	let formName = $state('');
	let formSlug = $state('');
	let formDescription = $state('');
	let formDossier = $state('');
	let formPiece = $state('');
	let pieces = $state<Piece[]>([]);
	let piecesLoading = $state(false);

	let historyOpen = $state(false);
	let historyLoading = $state(false);
	let detail = $state<SkillDetail | null>(null);

	const slugPreview = $derived(formSlug || slugify(formName));
	const formValid = $derived(
		formName.trim().length > 1 && slugPreview.length > 2 && formPiece !== ''
	);
	const dossierLabel = $derived(
		$dossiers.items.find((d) => d.id === formDossier)?.title ?? $t('common.none')
	);
	const pieceLabel = $derived(
		pieces.find((p) => p.id === formPiece)?.title ||
			pieces.find((p) => p.id === formPiece)?.original_name ||
			$t('common.none')
	);

	function slugify(value: string): string {
		return value
			.normalize('NFD')
			.replace(/[\u0300-\u036f]/g, '')
			.toLowerCase()
			.replace(/[^a-z0-9]+/g, '-')
			.replace(/^-+|-+$/g, '')
			.slice(0, 64);
	}

	function formatUnix(seconds: number | null | undefined): string {
		if (!seconds) return '—';
		return new Date(seconds * 1000).toLocaleDateString('fr-FR', {
			year: 'numeric',
			month: 'long',
			day: 'numeric'
		});
	}

	// ---- loading --------------------------------------------------------
	async function load() {
		try {
			const page = await listSkills({ limit: 100 });
			skills = page.items ?? [];
			error = '';
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('skills.loadError');
		} finally {
			loaded = true;
		}
	}

	onMount(() => {
		void load();
		void loadDossiers();
	});

	$effect(() => {
		const id = formDossier;
		if (!id) {
			pieces = [];
			return;
		}
		piecesLoading = true;
		listPieces(id, { limit: 200 })
			.then((page) => {
				pieces = page.items ?? [];
			})
			.catch(() => {
				pieces = [];
			})
			.finally(() => {
				piecesLoading = false;
			});
	});

	// ---- create from a model act ---------------------------------------
	function openCreate() {
		formName = '';
		formSlug = '';
		formDescription = '';
		formDossier = '';
		formPiece = '';
		createOpen = true;
	}

	async function submitCreate() {
		if (!formValid || submitting) return;
		submitting = true;
		error = '';
		const piece = pieces.find((p) => p.id === formPiece);
		try {
			await createSkill({
				slug: slugPreview,
				name: formName.trim(),
				description: formDescription.trim(),
				source_asset_id: piece?.source_asset_id ?? null
			});
			createOpen = false;
			await load();
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('skills.createError');
		} finally {
			submitting = false;
		}
	}

	// ---- history --------------------------------------------------------
	async function openHistory(skill: Skill) {
		historyOpen = true;
		historyLoading = true;
		detail = null;
		try {
			detail = await getSkill(skill.id);
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('skills.loadError');
			historyOpen = false;
		} finally {
			historyLoading = false;
		}
	}

	async function archive(skill: Skill) {
		if (!confirm($t('skills.deleteConfirm'))) return;
		try {
			await deleteSkill(skill.id);
			await load();
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('skills.loadError');
		}
	}

	// ---- the definition, read-only --------------------------------------
	type Definition = {
		sections?: { title: string; purpose: string; required: boolean }[];
		tone?: { register: string; person: string; notes: string };
		mandatory_clauses?: { label: string; text: string; why: string }[];
		checklist?: { item: string; blocking: boolean }[];
	};

	function definitionOf(version: SkillVersion): Definition {
		return (version.definition ?? {}) as Definition;
	}
</script>

<div in:fly={{ y: 12, duration: 400, easing: cubicOut }} class="mx-auto w-full max-w-6xl space-y-6 pb-6">
	<!-- ===== Header =================================================== -->
	<header class="flex flex-wrap items-end justify-between gap-x-6 gap-y-4">
		<div class="space-y-2">
			<h1 class="display text-foreground flex items-baseline gap-3 text-[40px]">
				{$t('skills.title')}
				{#if loaded}
					<span class="text-muted-foreground font-mono text-sm tracking-normal">{skills.length}</span>
				{/if}
			</h1>
			<p class="text-muted-foreground text-sm">{$t('skills.subtitle')}</p>
		</div>
		<Button class="h-9 px-4" onclick={openCreate}>
			<Icon name="add-circle" class="h-4 w-4" />
			{$t('skills.new')}
		</Button>
	</header>

	<div class="space-y-3">
		{#if error}
			<p class="text-destructive text-ui">{error}</p>
		{/if}

		<!-- ===== Body ==================================================== -->
		{#if !loaded}
			<div class="grid grid-cols-1 gap-3 py-2 sm:grid-cols-2 lg:grid-cols-3">
				{#each { length: 6 } as _}
					<Skeleton class="h-36 w-full rounded-2xl" />
				{/each}
			</div>
		{:else if skills.length === 0}
			<EmptyState icon="library" title={$t('skills.none')} hint={$t('skills.subtitle')}>
				<Button size="sm" onclick={openCreate}>
					<Icon name="add-circle" class="mr-1 h-3.5 w-3.5" />
					{$t('skills.new')}
				</Button>
			</EmptyState>
		{:else}
			<div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
				{#each skills as skill (skill.id)}
					<div class="motion-card bg-card group flex flex-col gap-2 rounded-2xl border p-5 shadow-xs">
						<div class="flex items-start justify-between gap-2">
							<div class="min-w-0 space-y-1">
								<div class="text-foreground truncate font-serif text-[19px] leading-tight">
									{skill.name}
								</div>
								<code class="text-muted-foreground font-mono text-2xs">/{skill.slug}</code>
							</div>
							<span
								class="inline-flex h-5 shrink-0 items-center gap-1.5 rounded-full px-2 text-2xs font-medium {skill.state ===
								'active'
									? 'bg-pos-soft text-pos-soft-foreground'
									: 'bg-muted text-muted-foreground'}"
							>
								<span
									class="h-1.5 w-1.5 rounded-full {skill.state === 'active' ? 'bg-pos' : 'bg-muted-foreground'}"
									aria-hidden="true"
								></span>
								{$t(`skills.state.${skill.state}`)}
							</span>
						</div>

						{#if skill.description}
							<p class="text-muted-foreground line-clamp-2 text-[11.5px]">{skill.description}</p>
						{/if}

						<div class="text-muted-foreground/80 flex flex-wrap items-center gap-x-3 text-[11px]">
							<span class="inline-flex items-center gap-1 tabular-nums">
								<Icon name="lock" class="h-3 w-3" />
								{$t('skills.version', tp({ number: skill.current_version }))}
							</span>
							<span class="tabular-nums">{formatUnix(skill.updated_at)}</span>
						</div>

						<div class="mt-auto flex items-center justify-between gap-1 pt-1">
							<Button
								variant="ghost"
								size="sm"
								class="text-muted-foreground hover:text-foreground h-7 text-[11.5px]"
								onclick={() => openHistory(skill)}
							>
								<Icon name="history" class="mr-1 h-3.5 w-3.5" />
								{$t('skills.versions')}
							</Button>
							<Tooltip.Root>
								<Tooltip.Trigger>
									<Button
										variant="ghost"
										size="icon"
										class="text-muted-foreground hover:text-destructive hover:bg-destructive/10 h-7 w-7 opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100"
										onclick={() => archive(skill)}
										aria-label={$t('skills.delete')}
									>
										<Icon name="trash-bin-trash" class="h-3.5 w-3.5" />
									</Button>
								</Tooltip.Trigger>
								<Tooltip.Content>{$t('skills.delete')}</Tooltip.Content>
							</Tooltip.Root>
						</div>
					</div>
				{/each}
			</div>
		{/if}

		<p class="text-muted-foreground/80 flex items-start gap-1.5 text-[11px]">
			<Icon name="lock" class="mt-px h-3 w-3 shrink-0" />
			{$t('skills.immutableHint')}
		</p>
	</div>
</div>

<!-- ===== Create from a model act (right-side Sheet, per the canon) ====== -->
<Sheet.Root bind:open={createOpen}>
	<Sheet.Content side="right" class="w-full sm:max-w-md">
		<Sheet.Header>
			<Sheet.Title class="text-[14px]">{$t('skills.new')}</Sheet.Title>
			<Sheet.Description class="text-[12px]">{$t('skills.sourceActHint')}</Sheet.Description>
		</Sheet.Header>

		<form
			class="space-y-4 px-4"
			onsubmit={(e) => {
				e.preventDefault();
				submitCreate();
			}}
		>
			<div class="space-y-3">
				<div
					class="text-muted-foreground/70 text-[11px] font-medium uppercase tracking-wider"
				>
					{$t('skills.name')}
				</div>
				<div class="space-y-1.5">
					<Label for="skill-name" class="text-muted-foreground text-[11.5px]">
						{$t('skills.name')}
					</Label>
					<Input id="skill-name" bind:value={formName} placeholder={$t('skills.title')} />
				</div>
				<div class="space-y-1.5">
					<Label for="skill-slug" class="text-muted-foreground text-[11.5px]">
						{$t('skills.slug')}
					</Label>
					<Input
						id="skill-slug"
						value={slugPreview}
						oninput={(e) => (formSlug = slugify((e.target as HTMLInputElement).value))}
						class="font-mono"
					/>
					<p class="text-muted-foreground/80 text-[11px]">
						{$t('skills.trigger')} · <code class="font-mono">/{slugPreview}</code>
					</p>
				</div>
				<div class="space-y-1.5">
					<Label for="skill-desc" class="text-muted-foreground text-[11.5px]">
						{$t('skills.description')}
					</Label>
					<Textarea
						id="skill-desc"
						bind:value={formDescription}
						class="min-h-[72px] text-[12.5px]"
					/>
				</div>
			</div>

			<Separator class="border-dashed" />

			<div class="space-y-3">
				<div
					class="text-muted-foreground/70 text-[11px] font-medium uppercase tracking-wider"
				>
					{$t('skills.sourceAct')}
				</div>
				<div class="space-y-1.5">
					<Label class="text-muted-foreground text-[11.5px]">{$t('skills.pickDossier')}</Label>
					<Select.Root type="single" bind:value={formDossier}>
						<Select.Trigger class="text-[12.5px]">{dossierLabel}</Select.Trigger>
						<Select.Content>
							{#each $dossiers.items as d (d.id)}
								<Select.Item value={d.id}>{d.title}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
				</div>
				<div class="space-y-1.5">
					<Label class="text-muted-foreground text-[11.5px]">{$t('skills.pickPiece')}</Label>
					<Select.Root type="single" bind:value={formPiece} disabled={pieces.length === 0}>
						<Select.Trigger class="text-[12.5px]">{pieceLabel}</Select.Trigger>
						<Select.Content>
							{#each pieces as p (p.id)}
								<Select.Item value={p.id}>{p.title || p.original_name}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
					{#if formDossier && !piecesLoading && pieces.length === 0}
						<p class="text-muted-foreground/80 text-[11px]">{$t('skills.noPieces')}</p>
					{/if}
				</div>
			</div>

			{#if error}
				<p class="text-destructive text-[12.5px]">{error}</p>
			{/if}
		</form>

		<Sheet.Footer class="flex-row items-center justify-between gap-3">
			<Button variant="ghost" size="sm" type="button" onclick={() => (createOpen = false)}>
				<Icon name="close-circle" class="mr-1 h-3.5 w-3.5" />
				{$t('common.cancel')}
			</Button>
			<Button size="sm" disabled={!formValid || submitting} onclick={submitCreate}>
				{#if submitting}
					<Spinner class="mr-1 h-3.5 w-3.5" />
				{:else}
					<Icon name="diskette" class="mr-1 h-3.5 w-3.5" />
				{/if}
				{submitting ? $t('skills.extracting') : $t('skills.create')}
			</Button>
		</Sheet.Footer>
	</Sheet.Content>
</Sheet.Root>

<!-- ===== Version history — the proof of immutability =================== -->
<Dialog.Root bind:open={historyOpen}>
	<Dialog.Content class="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
		<Dialog.Header>
			<Dialog.Title class="text-[14px]">
				{detail ? detail.name : $t('skills.versions')}
			</Dialog.Title>
			<Dialog.Description class="text-[12px]">{$t('skills.immutableHint')}</Dialog.Description>
		</Dialog.Header>

		{#if historyLoading}
			<div class="space-y-2 py-2">
				{#each { length: 3 } as _}
					<Skeleton class="h-16 w-full" />
				{/each}
			</div>
		{:else if detail}
			<div class="space-y-3">
				{#each detail.versions as version (version.id)}
					{@const def = definitionOf(version)}
					{@const current = version.version === detail.current_version}
					<div
						class="border-border/80 space-y-2 rounded-lg border border-dashed p-3 {current
							? 'ring-primary/30 ring-1'
							: ''}"
					>
						<div class="flex flex-wrap items-center justify-between gap-2">
							<div class="flex items-center gap-2">
								<Badge variant={current ? 'default' : 'outline'} class="text-[10px]">
									{$t('skills.version', tp({ number: version.version }))}
								</Badge>
								{#if current}
									<span class="text-muted-foreground text-[11px]">
										{$t('skills.currentVersion')}
									</span>
								{/if}
							</div>
							<span class="text-muted-foreground/80 text-[11px] tabular-nums">
								{formatUnix(version.created_at)}
							</span>
						</div>

						<div class="text-muted-foreground/80 flex flex-wrap items-center gap-x-4 text-[11px]">
							<span>{$t('skills.extractorModel')} · {version.extractor_model || '—'}</span>
							{#if version.source_asset_id}
								<span class="inline-flex items-center gap-1">
									<Icon name="file-text" class="h-3 w-3" />
									{$t('skills.sourceAct')}
								</span>
							{/if}
						</div>

						{#if def.sections?.length}
							<div class="space-y-1">
								<div
									class="text-muted-foreground/70 text-[10px] font-semibold uppercase tracking-wider"
								>
									{$t('skills.definition.sections')}
								</div>
								<ul class="space-y-0.5">
									{#each def.sections as section}
										<li class="text-[11.5px]">
											<span class="font-medium">{section.title}</span>
											{#if section.required}
												<span class="text-muted-foreground/70">
													· {$t('skills.definition.required')}
												</span>
											{/if}
										</li>
									{/each}
								</ul>
							</div>
						{/if}

						{#if def.tone}
							<div class="text-muted-foreground text-[11.5px]">
								<span class="text-muted-foreground/70 text-[10px] font-semibold uppercase tracking-wider">
									{$t('skills.definition.tone')}
								</span>
								· {def.tone.register} · {def.tone.person}
							</div>
						{/if}

						{#if def.mandatory_clauses?.length}
							<div class="space-y-1">
								<div
									class="text-muted-foreground/70 text-[10px] font-semibold uppercase tracking-wider"
								>
									{$t('skills.definition.clauses')}
								</div>
								<ul class="space-y-0.5">
									{#each def.mandatory_clauses as clause}
										<li class="text-[11.5px]">{clause.label}</li>
									{/each}
								</ul>
							</div>
						{/if}

						{#if def.checklist?.length}
							<div class="space-y-1">
								<div
									class="text-muted-foreground/70 text-[10px] font-semibold uppercase tracking-wider"
								>
									{$t('skills.definition.checklist')}
								</div>
								<ul class="space-y-0.5">
									{#each def.checklist as check}
										<li class="text-[11.5px]">
											{check.item}
											{#if check.blocking}
												<span class="text-destructive/80">
													· {$t('skills.definition.blocking')}
												</span>
											{/if}
										</li>
									{/each}
								</ul>
							</div>
						{/if}
					</div>
				{/each}
			</div>
		{/if}

		<Dialog.Footer>
			<Button variant="ghost" size="sm" onclick={() => (historyOpen = false)}>
				{$t('common.close')}
			</Button>
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>
