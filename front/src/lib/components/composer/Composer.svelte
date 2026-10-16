<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	import { onMount, tick } from 'svelte';
	import { fade } from 'svelte/transition';
	import { Portal } from 'bits-ui';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import * as Command from '$lib/components/ui/command';
	import { Button } from '$lib/components/ui/button';
	import { Textarea } from '$lib/components/ui/textarea';
	import { FileDropZone, displaySize } from '$lib/components/ui/file-drop-zone';
	import { fileStyle } from '$lib/utils/file-style';
	import { t } from '$lib/i18n';
	import { listSkills } from '$lib/api/skills';
	import { BorderBeam } from '$lib/components/libraries/border-beam';
	import { stageFiles } from './stage';
	import type { Attachment, ComposerPayload, ComposerSkill } from './types';

	/**
	 * The composer (docs/PLAN.md §11). One component, two hosts: the home
	 * (`/chat`) mounts it in the centre of the canvas, the thread
	 * (`/chat/[id]`) pins it at the bottom. Everything it produces travels in a
	 * single `onsubmit` payload, and everything it holds is bindable, so the
	 * host owns the state and decides when to clear it — a failed send must
	 * never silently eat the instruction the lawyer typed.
	 *
	 * Files dropped anywhere on the page, or picked with the paperclip, are
	 * staged here as attachments carrying their `File`; the host files them
	 * into the dossier when the turn is sent (see `stage.ts`).
	 */
	let {
		activeTurn = false,
		value = $bindable(''),
		attachments = $bindable([]),
		skill = $bindable(null),
		collectionTitle = null,
		placeholder = '',
		busy = false,
		live = false,
		autofocus = false,
		skills,
		class: className = '',
		onsubmit
	}: {
		activeTurn?: boolean;
		value?: string;
		attachments?: Attachment[];
		skill?: string | null;
		/** The bound dossier, which staged files go into; null when sending creates one. */
		collectionTitle?: string | null;
		placeholder?: string;
		busy?: boolean;
		/** A mission is running in this thread: light travels the border. */
		live?: boolean;
		autofocus?: boolean;
		/** Optional: the host's compétence list. Omitted, the composer loads it. */
		skills?: ComposerSkill[];
		class?: string;
		onsubmit?: (payload: ComposerPayload) => void;
	} = $props();

	// sveltekit-i18n types the default payload as empty, so an interpolated key
	// (`{{count}}`, `{{name}}`) needs a widened signature until the shared
	// config declares one.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);

	const hintId = $props.id();

	// The agent chooses its own model and tools: the model/tone preferences an
	// earlier composer persisted are dropped, never read into a request.
	onMount(() => {
		try {
			localStorage.removeItem('nomos.composer');
		} catch {
			/* storage may be disabled */
		}
	});

	let inputEl = $state<HTMLTextAreaElement | null>(null);
	let slashOpen = $state(false);
	let slashIndex = $state(0);
	let loadedSkills = $state<ComposerSkill[]>([]);

	const resolvedPlaceholder = $derived(placeholder || $t('chat.agent.placeholder'));
	const skillList = $derived(skills ?? loadedSkills);
	const activeSkill = $derived(skillList.find((s) => s.slug === skill) ?? null);
	// A turn needs an instruction: files alone say nothing about what to do with them.
	const canSubmit = $derived(!busy && !activeTurn && value.trim().length > 0);

	const disabledHint = $derived(
		activeTurn
			? 'chat.agent.activeTurn'
			: busy
				? 'chat.agent.sending'
				: 'chat.agent.emptyInstruction'
	);

	// --- attachments ------------------------------------------------------
	const stagedCount = $derived(attachments.filter((a) => a.file).length);
	/** Where staged files go when the turn is sent. */
	const stagedDestination = $derived(
		collectionTitle
			? tx('chat.composer.stagedInto', { name: collectionTitle })
			: $t('chat.composer.stagedNew')
	);
	/** One icon per distinct file type, capped — the strip is a glance, not a list. */
	const typeIcons = $derived.by(() => {
		const seen = new Map<string, ReturnType<typeof fileStyle>>();
		for (const a of attachments) {
			const ext = a.name.split('.').pop()?.toLowerCase() ?? '';
			if (!seen.has(ext)) seen.set(ext, fileStyle(a.name));
		}
		return [...seen.entries()].slice(0, 4);
	});

	function removeAttachment(id: string) {
		attachments = attachments.filter((a) => a.id !== id);
	}

	// --- compétences, and the `/` menu that shares their list --------------
	onMount(async () => {
		if (skills) return;
		try {
			const page = await listSkills({ limit: 100 });
			loadedSkills = (page.items ?? [])
				.filter((s) => s.state !== 'archived')
				.map((s) => ({ slug: s.slug, name: s.name, description: s.description }));
		} catch {
			// The compétences endpoint may not be reachable in this build; the
			// selector then shows its empty state rather than an error.
			loadedSkills = [];
		}
	});

	const slashQuery = $derived(value.startsWith('/') ? value.slice(1).trim().toLowerCase() : '');
	const slashMatches = $derived(
		slashQuery
			? skillList.filter(
					(s) =>
						s.name.toLowerCase().includes(slashQuery) || s.slug.toLowerCase().includes(slashQuery)
				)
			: skillList
	);

	function onInput() {
		const open = value.startsWith('/');
		if (open !== slashOpen) slashIndex = 0;
		slashOpen = open;
	}

	async function pickSkill(slug: string) {
		skill = slug;
		if (value.startsWith('/')) value = '';
		slashOpen = false;
		slashIndex = 0;
		await tick();
		inputEl?.focus();
	}

	// --- submit ------------------------------------------------------------
	function submit() {
		if (!canSubmit) return;
		slashOpen = false;
		onsubmit?.({ text: value.trim(), attachments: [...attachments], skill });
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.isComposing) return;
		if (slashOpen && slashMatches.length > 0) {
			if (e.key === 'ArrowDown') {
				e.preventDefault();
				slashIndex = (slashIndex + 1) % slashMatches.length;
				return;
			}
			if (e.key === 'ArrowUp') {
				e.preventDefault();
				slashIndex = (slashIndex - 1 + slashMatches.length) % slashMatches.length;
				return;
			}
			if (e.key === 'Enter' || e.key === 'Tab') {
				e.preventDefault();
				pickSkill(slashMatches[slashIndex].slug);
				return;
			}
			if (e.key === 'Escape') {
				e.preventDefault();
				slashOpen = false;
				return;
			}
		}
		if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
			e.preventDefault();
			submit();
		}
	}

	async function handleAttach(files: File[]) {
		if (files.length) attachments = [...attachments, ...stageFiles(files)];
	}

	// --- drop anywhere --------------------------------------------------------
	// Files dragged over the window stage wherever they land: the lawyer
	// should not have to aim for the paperclip. The depth counter survives
	// the enter/leave pairs every child element fires on the way.
	let dragDepth = $state(0);

	function carriesFiles(e: DragEvent) {
		return !!e.dataTransfer?.types.includes('Files');
	}

	function onDragEnter(e: DragEvent) {
		if (!carriesFiles(e)) return;
		e.preventDefault();
		dragDepth += 1;
	}

	function onDragOver(e: DragEvent) {
		if (!carriesFiles(e) || !e.dataTransfer) return;
		// Cancelled, or the browser opens the file in place of the app.
		e.preventDefault();
		e.dataTransfer.dropEffect = busy ? 'none' : 'copy';
	}

	function onDragLeave(e: DragEvent) {
		if (carriesFiles(e)) dragDepth = Math.max(0, dragDepth - 1);
	}

	function onDrop(e: DragEvent) {
		if (!carriesFiles(e)) return;
		dragDepth = 0;
		// The paperclip's own drop zone already took them.
		if (e.defaultPrevented) return;
		e.preventDefault();
		if (!busy) void handleAttach(Array.from(e.dataTransfer?.files ?? []));
	}

	// --- shape ---------------------------------------------------------------
	// The composer rests as one line — paperclip, instruction, send —
	// and opens into a full-width editor once the text wraps. It closes only
	// when emptied, so a sentence sitting at the wrap point cannot flicker
	// between the two shapes. The card's height eases between them.
	/** One line of the instruction: `min-h-11`. */
	const ONE_LINE = 44;
	let multiline = $state(false);
	/** Set at the first change of shape, so the controls settle only after a real one. */
	let reshaped = $state(false);
	let contentHeight = $state(0);
	const pill = $derived(!multiline && attachments.length === 0 && !skill);

	// Auto-grow: reset then measure, capped so a long instruction scrolls
	// instead of pushing the toolbar off screen. The same measure decides the
	// shape: a second line opens the editor.
	$effect(() => {
		const text = value;
		void multiline; // the editor's width changes with its shape
		if (!inputEl) return;
		inputEl.style.height = 'auto';
		const full = inputEl.scrollHeight;
		inputEl.style.height = `${Math.min(full, 256)}px`;
		const open = text.length > 0 && (multiline || text.includes('\n') || full > ONE_LINE + 4);
		if (open !== multiline) {
			multiline = open;
			reshaped = true;
		}
	});

	$effect(() => {
		if (autofocus && inputEl) inputEl.focus();
	});
</script>

<svelte:window
	ondragenter={onDragEnter}
	ondragover={onDragOver}
	ondragleave={onDragLeave}
	ondrop={onDrop}
/>

{#if dragDepth > 0 && !busy}
	<Portal>
		<div
			transition:fade={{ duration: 120 }}
			class="bg-background/70 pointer-events-none fixed inset-0 z-50 flex items-center justify-center p-6 backdrop-blur-[2px]"
		>
			<div class="bg-card flex max-w-sm flex-col items-center gap-3 rounded-2xl border px-10 py-8 text-center shadow-lg">
				<span class="bg-brand-soft text-brand flex h-11 w-11 items-center justify-center rounded-xl">
					<Icon name="document-add" active class="h-5 w-5" />
				</span>
				<p class="text-foreground text-sm font-semibold">{$t('chat.composer.dropTitle')}</p>
				<p class="text-muted-foreground text-xs leading-relaxed">{stagedDestination}</p>
			</div>
		</div>
	</Portal>
{/if}

{#snippet instruction()}
	<Textarea
		bind:ref={inputEl}
		bind:value
		rows={1}
		oninput={onInput}
		onkeydown={onKeydown}
		disabled={busy}
		placeholder={resolvedPlaceholder}
		aria-label={$t('chat.agent.placeholder')}
		class="min-h-11 max-h-64 overflow-y-auto px-2 py-2.5 leading-6 dark:bg-transparent resize-none border-0 bg-transparent text-read shadow-none placeholder:text-muted-foreground/70 focus-visible:border-0 focus-visible:ring-0 md:text-read"
	/>
{/snippet}

{#snippet attachControl()}
		<Tooltip.Root>
		<Tooltip.Trigger>
			{#snippet child({ props })}
				<div {...props}>
					<FileDropZone
						onUpload={handleAttach}
						disabled={busy}
						aria-label={$t('chat.composer.attach')}
						class="h-11 w-11 rounded-full transition-colors duration-200 place-items-center border-0 p-0 text-muted-foreground hover:bg-accent hover:text-foreground"
					>
						<Icon name="paperclip" class="h-4 w-4" />
					</FileDropZone>
				</div>
			{/snippet}
		</Tooltip.Trigger>
		<Tooltip.Content class="bg-card text-foreground" arrowClasses="bg-card">
			{$t('chat.composer.attach')}
		</Tooltip.Content>
	</Tooltip.Root>
{/snippet}

{#snippet sendControl()}
	<Button
		size="icon"
		aria-describedby={!canSubmit ? hintId : undefined}
		disabled={!canSubmit}
		onclick={submit}
		aria-label={$t('chat.composer.send')}
		class="ml-1 focus-visible:ring-brand ring-offset-background h-11 w-11 transition-[background-color,color,scale] duration-300 focus-visible:ring-2 active:scale-95 rounded-full disabled:bg-muted disabled:bg-none disabled:text-muted-foreground disabled:opacity-100 disabled:shadow-none"
	>
		{#if busy}
			<Spinner class="h-4 w-4" />
		{:else}
			<Icon name="arrow-up" class="h-4 w-4" />
		{/if}
	</Button>
{/snippet}

<div class="relative w-full {className}" data-tour="composer">
	<!-- Slash menu: the compétences list, anchored above the input -->
	{#if slashOpen}
		<div class="absolute bottom-full left-0 right-0 z-30 mb-2">
			<Command.Root
				class="max-h-72 overflow-hidden rounded-2xl border border-border bg-popover shadow-lg"
			>
				<Command.List class="max-h-72">
					{#if slashMatches.length === 0}
						<div class="px-3 py-6 text-center text-xs text-muted-foreground">
							{$t('chat.composer.noSkills')}
						</div>
					{:else}
						<Command.Group heading={$t('chat.composer.skills')}>
							{#each slashMatches as item, i (item.slug)}
								<Command.Item
									value={item.slug}
									onSelect={() => pickSkill(item.slug)}
									class={i === slashIndex ? 'bg-accent text-accent-foreground' : ''}
								>
									<Icon name="square-academic-cap" class="h-3.5 w-3.5" />
									<span class="font-medium">{item.name}</span>
									{#if item.description}
										<span class="truncate text-2xs text-muted-foreground">
											{item.description}
										</span>
									{/if}
									<Command.Shortcut class="font-mono text-2xs text-muted-foreground/70">
										/{item.slug}
									</Command.Shortcut>
								</Command.Item>
							{/each}
						</Command.Group>
					{/if}
				</Command.List>
			</Command.Root>
		</div>
	{/if}

	<!-- The beam clips to its own box, so the shadow lives on it, not the card -->
	<BorderBeam
		active={live}
		borderRadius={pill ? 28 : 22}
		class="shadow-sm transition-[box-shadow,border-radius] duration-500 focus-within:shadow-md"
	>
		<div
			class="relative border border-border bg-card transition-[border-color,border-radius] duration-500 ease-[var(--motion-spring)] focus-within:border-foreground/20 {pill
				? 'rounded-[28px]'
				: 'rounded-[22px]'}"
		>
			<!-- The content changes height at once; this box eases after it. -->
			<div
				class="overflow-hidden transition-[height] duration-500 ease-[var(--motion-spring)] motion-reduce:transition-none"
				style:height={contentHeight ? `${contentHeight}px` : undefined}
			>
			<div bind:clientHeight={contentHeight}>
			<!-- Attachment strip: what is attached -->
			{#if attachments.length > 0}
				<div class="space-y-2 border-b border-border px-3.5 py-3">
					<div class="flex items-center gap-2">
						<div class="flex items-center -space-x-1.5">
							{#each typeIcons as [ext, style] (ext)}
								<span
									class="flex h-6 w-6 items-center justify-center rounded-lg border border-card {style.bg} {style.text}"
								>
									<Icon name={style.icon} class="h-3 w-3" />
								</span>
							{/each}
						</div>
						<span class="text-ui font-medium text-foreground">
							{tx('chat.composer.documentsInContext', { count: attachments.length })}
						</span>
					</div>
					<div class="flex flex-wrap gap-1.5">
						{#each attachments as file (file.id)}
							{@const style = fileStyle(file.name)}
							<span
								class="flex max-w-[220px] items-center gap-1.5 rounded-full border border-border bg-background px-2.5 py-1 text-xs"
							>
								<!-- Every staged file is sent. -->
								<Icon name={style.icon} class="h-3 w-3 shrink-0 {style.text}" />
								<span class="truncate text-foreground" title={file.name}>{file.name}</span>
								<span class="shrink-0 text-muted-foreground">{displaySize(file.size)}</span>
								<button
									type="button"
									onclick={() => removeAttachment(file.id)}
									aria-label={$t('chat.composer.removeAttachment')}
									class="focus-visible:ring-brand ring-offset-background relative rounded-full after:absolute after:-inset-4 focus-visible:ring-2 focus-visible:outline-none shrink-0 cursor-pointer text-muted-foreground transition-colors hover:text-foreground"
								>
									<Icon name="close-circle" class="h-3 w-3" />
								</button>
							</span>
						{/each}
					</div>
					{#if stagedCount}
						<p class="text-muted-foreground text-2xs">{stagedDestination}</p>
					{/if}
				</div>
			{/if}

			<!-- The `/` pick, as plain context: a template, not a mission chip -->
			{#if skill}
				<div class="text-muted-foreground flex min-w-0 items-center gap-2 px-4.5 pt-3 text-xs">
					<Icon name="square-academic-cap" class="h-3.5 w-3.5 shrink-0" />
					<span class="min-w-0 truncate" title={activeSkill?.name ?? skill}>
						{activeSkill?.name ?? skill}
					</span>
					<button
						type="button"
						onclick={() => (skill = null)}
						aria-label={$t('chat.composer.clearSkill')}
						class="hover:text-foreground focus-visible:ring-brand ring-offset-background relative flex h-6 w-6 shrink-0 cursor-pointer items-center justify-center rounded-full transition-colors after:absolute after:-inset-2.5 focus-visible:ring-2 focus-visible:outline-none"
					>
						<Icon name="close-circle" class="h-3.5 w-3.5" />
					</button>
				</div>
			{/if}

			<!-- One line at rest, a full-width editor once the text wraps -->
			<div
				class="composer-row grid grid-cols-[auto_minmax(0,1fr)_auto] gap-x-1 p-1.5 {multiline
					? 'items-end gap-y-0.5'
					: 'items-center'}"
				data-shape={reshaped ? (multiline ? 'open' : 'line') : undefined}
			>
				<div
					class="composer-cell row-start-1 min-w-0 {multiline
						? 'col-span-3 col-start-1 px-1.5 pt-1.5'
						: 'col-start-2'}"
				>
					{@render instruction()}
				</div>
				<div class="composer-tool col-start-1 {multiline ? 'row-start-2' : 'row-start-1'}">
					{@render attachControl()}
				</div>
				<div class="composer-tool col-start-3 {multiline ? 'row-start-2' : 'row-start-1'}">
					{@render sendControl()}
				</div>
			</div>
			</div>
			</div>
		</div>
	</BorderBeam>
	<!-- Why send is disabled. Only the active-turn conflict needs to be seen;
	     the empty and in-flight cases are already visible, so they stay for
	     assistive technology. -->
	{#if !canSubmit}
		<p id={hintId} class={activeTurn ? 'text-muted-foreground mt-2 text-xs' : 'sr-only'}>
			{$t(disabledHint)}
		</p>
	{/if}
</div>

<style>
	/* A change of shape moves the controls and the text at once; they settle
	   into their new place instead of jumping. Two keyframe names per element,
	   so each change restarts its animation. */
	@media (prefers-reduced-motion: no-preference) {
		.composer-row[data-shape='open'] .composer-tool {
			animation: composer-tool-open 460ms var(--motion-spring) both;
		}
		.composer-row[data-shape='line'] .composer-tool {
			animation: composer-tool-line 460ms var(--motion-spring) both;
		}
		.composer-row[data-shape='open'] .composer-cell {
			animation: composer-text-open 380ms var(--motion-spring) both;
		}
		.composer-row[data-shape='line'] .composer-cell {
			animation: composer-text-line 380ms var(--motion-spring) both;
		}
	}
	@keyframes composer-tool-open {
		from {
			opacity: 0.25;
			translate: 0 -10px;
		}
	}
	@keyframes composer-tool-line {
		from {
			opacity: 0.25;
			translate: 0 10px;
		}
	}
	@keyframes composer-text-open {
		from {
			opacity: 0.35;
			translate: 0 4px;
		}
	}
	@keyframes composer-text-line {
		from {
			opacity: 0.35;
		}
	}
</style>
