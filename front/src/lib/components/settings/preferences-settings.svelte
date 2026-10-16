<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// =====================================================================
	// The preferences tab of /settings — language, style preferences and
	// memory management (§8, §11).
	//
	// The form-page layout: one Card.Root per section, sections
	// split by a dashed Separator, `text-[11.5px] text-muted-foreground`
	// labels, Cancel (ghost, left) + Save (default, right) footers.
	//
	// What the page has to make impossible to miss: memory is EXPLICIT.
	// Every stored preference shows its provenance — approved by the user,
	// or merely inferred by the model — and an inferred entry is inert until
	// a human approves it. Nothing here turns conversation text into firm
	// memory on its own; that is a consent problem before it is a feature.
	// =====================================================================

	import { onMount } from 'svelte';
	import * as Card from '$lib/components/ui/card';
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
	import { t, locale, setLocale, loadTranslations, resolveLocale } from '$lib/i18n';
	import { user } from '$lib/stores/user';
	import {
		listMemory,
		upsertMemory,
		approveMemory,
		deleteMemory,
		type MemoryPreference,
		type MemoryScope
	} from '$lib/api/memory';

	// ---- language -------------------------------------------------------
	// The same key the shell writes, so the toggle here and the one in the
	// user popover can never disagree.
	const LANG_KEY = 'nomos.lang';
	const languages = [
		{ value: 'fr', code: 'FR', labelKey: 'nav.fr' },
		{ value: 'en', code: 'EN', labelKey: 'nav.en' }
	];

	function applyLocale(next: string) {
		const value = resolveLocale(next);
		void loadTranslations(value);
		setLocale(value);
		try {
			localStorage.setItem(LANG_KEY, value);
		} catch {
			/* private mode: the language still applies for this session */
		}
		if ($user.settings) $user.settings.lang = value;
	}

	// ---- memory ---------------------------------------------------------
	let loaded = $state(false);
	let entries = $state<MemoryPreference[]>([]);
	let error = $state('');
	let busyId = $state('');

	let formScope = $state<MemoryScope>('user');
	let formKey = $state('');
	let formValue = $state('');
	let submitting = $state(false);

	const scopeOptions: { value: MemoryScope; labelKey: string }[] = [
		{ value: 'user', labelKey: 'memory.scopes.user' },
		{ value: 'firm', labelKey: 'memory.scopes.firm' }
	];
	const scopeLabel = $derived(
		$t(scopeOptions.find((o) => o.value === formScope)?.labelKey ?? 'memory.scopes.user')
	);
	const formValid = $derived(formKey.trim().length > 1 && formValue.trim().length > 0);

	const approved = $derived(entries.filter((e) => e.state === 'approved'));
	const pending = $derived(entries.filter((e) => e.state !== 'approved'));

	function textOf(entry: MemoryPreference): string {
		const value = entry.value as { text?: string } | undefined;
		if (value && typeof value.text === 'string') return value.text;
		return entry.value ? JSON.stringify(entry.value) : '';
	}

	function formatUnix(seconds: number | null | undefined): string {
		if (!seconds) return '—';
		return new Date(seconds * 1000).toLocaleDateString('fr-FR', {
			year: 'numeric',
			month: 'long',
			day: 'numeric'
		});
	}

	async function load() {
		try {
			const page = await listMemory({ limit: 200 });
			entries = page.items ?? [];
			error = '';
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('memory.loadError');
		} finally {
			loaded = true;
		}
	}

	onMount(() => {
		void load();
	});

	async function addPreference() {
		if (!formValid || submitting) return;
		submitting = true;
		error = '';
		try {
			await upsertMemory({
				scope: formScope,
				key: formKey.trim(),
				value: { text: formValue.trim() },
				provenance: 'user_approved'
			});
			formKey = '';
			formValue = '';
			await load();
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('memory.saveError');
		} finally {
			submitting = false;
		}
	}

	/** The only path from inferred to used, and always a human action. */
	async function approve(entry: MemoryPreference) {
		busyId = entry.id;
		try {
			await approveMemory(entry);
			await load();
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('memory.saveError');
		} finally {
			busyId = '';
		}
	}

	async function remove(entry: MemoryPreference) {
		busyId = entry.id;
		try {
			await deleteMemory(entry.id);
			await load();
		} catch (err) {
			error = (err as { message?: string }).message ?? $t('memory.saveError');
		} finally {
			busyId = '';
		}
	}
</script>

{#snippet entryRow(entry: MemoryPreference)}
	<div
		class="border-border/80 flex flex-wrap items-start justify-between gap-3 rounded-md border border-dashed p-3"
	>
		<div class="min-w-0 flex-1 space-y-1">
			<div class="flex flex-wrap items-center gap-1.5">
				<code class="font-mono text-[11.5px] font-medium">{entry.key}</code>
				<Badge variant="outline" class="text-[10px]">
					{$t(`memory.scopes.${entry.scope}`)}
				</Badge>
				<Badge
					variant={entry.provenance === 'user_approved' ? 'default' : 'secondary'}
					class="text-[10px]"
				>
					{#if entry.provenance === 'user_approved'}
						<Icon name="shield-check" class="mr-1 h-3 w-3" />
					{:else}
						<Icon name="stars" class="mr-1 h-3 w-3" />
					{/if}
					{$t(`memory.provenances.${entry.provenance}`)}
				</Badge>
				<span class="text-muted-foreground/70 text-[10.5px]">
					{$t(`memory.state.${entry.state}`)}
				</span>
			</div>
			<p class="text-muted-foreground text-[12px]">{textOf(entry)}</p>
			<p class="text-muted-foreground/70 text-[10.5px] tabular-nums">
				{entry.approved_at ? `${$t('memory.approve')} · ${formatUnix(entry.approved_at)}` : ''}
				{entry.expires_at ? ` · ${$t('memory.expiresAt')} ${formatUnix(entry.expires_at)}` : ''}
			</p>
		</div>
		<div class="flex items-center gap-0.5">
			{#if entry.state !== 'approved'}
				<Button
					variant="ghost"
					size="sm"
					class="h-7 text-[11.5px]"
					disabled={busyId === entry.id}
					onclick={() => approve(entry)}
				>
					<Icon name="check-circle" class="mr-1 h-3.5 w-3.5" />
					{$t('memory.approve')}
				</Button>
			{/if}
			<Tooltip.Root>
				<Tooltip.Trigger>
					<Button
						variant="ghost"
						size="icon"
						class="text-destructive hover:text-destructive h-7 w-7"
						disabled={busyId === entry.id}
						onclick={() => remove(entry)}
						aria-label={$t('memory.delete')}
					>
						<Icon name="trash-bin-trash" class="h-3.5 w-3.5" />
					</Button>
				</Tooltip.Trigger>
				<Tooltip.Content>{$t('memory.delete')}</Tooltip.Content>
			</Tooltip.Root>
		</div>
	</div>
{/snippet}

<div class="space-y-4">
	<!-- ===== Language ================================================= -->
	<Card.Root class="gap-4 rounded-2xl p-5">
		<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
			<Icon name="translation" class="text-muted-foreground h-4 w-4" />
			{$t('nav.language')}
		</div>
		<div class="flex flex-wrap items-center justify-between gap-3">
			<p class="text-muted-foreground text-[11.5px]">{$t('settings.languageHint')}</p>
			<div class="bg-muted/50 grid grid-cols-2 gap-1 rounded-lg p-1">
				{#each languages as option}
					{@const active = $locale === option.value}
					<button
						type="button"
						onclick={() => applyLocale(option.value)}
						class="flex items-center justify-center gap-1.5 rounded-md px-3 py-1.5 text-[11.5px] font-medium transition-all duration-150 {active
							? 'bg-card text-foreground border-border/60 border shadow-sm'
							: 'text-muted-foreground hover:text-foreground'}"
					>
						<span class="font-mono text-[10px] font-bold opacity-70">{option.code}</span>
						<span>{$t(option.labelKey)}</span>
					</button>
				{/each}
			</div>
		</div>
	</Card.Root>

	<!-- ===== Add a style preference =================================== -->
	<Card.Root class="gap-4 rounded-2xl p-5">
		<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
			<Icon name="brain" class="text-muted-foreground h-4 w-4" />
			{$t('settings.preferencesTitle')}
		</div>
		<p class="text-muted-foreground text-[11.5px]">{$t('memory.subtitle')}</p>

		<form
			class="contents"
			onsubmit={(e) => {
				e.preventDefault();
				addPreference();
			}}
		>
			<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
				<div class="space-y-1.5">
					<Label for="mem-key" class="text-muted-foreground text-[11.5px]">
						{$t('memory.key')}
					</Label>
					<Input
						id="mem-key"
						bind:value={formKey}
						placeholder={$t('settings.preferenceKeyPlaceholder')}
						class="font-mono"
					/>
				</div>
				<div class="space-y-1.5">
					<Label class="text-muted-foreground text-[11.5px]">{$t('memory.scope')}</Label>
					<Select.Root type="single" bind:value={formScope}>
						<Select.Trigger class="text-[12.5px]">{scopeLabel}</Select.Trigger>
						<Select.Content>
							{#each scopeOptions as option}
								<Select.Item value={option.value}>{$t(option.labelKey)}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
				</div>
			</div>
			<div class="space-y-1.5">
				<Label for="mem-value" class="text-muted-foreground text-[11.5px]">
					{$t('memory.value')}
				</Label>
				<Textarea
					id="mem-value"
					bind:value={formValue}
					placeholder={$t('settings.preferenceValuePlaceholder')}
					class="min-h-[72px] text-[12.5px]"
				/>
			</div>

			{#if error}
				<p class="text-destructive text-[12.5px]">{error}</p>
			{/if}

			<Separator class="border-dashed" />

			<div class="flex items-center justify-between gap-3">
				<Button
					variant="ghost"
					size="sm"
					type="button"
					onclick={() => {
						formKey = '';
						formValue = '';
					}}
				>
					<Icon name="close-circle" class="mr-1 h-3.5 w-3.5" />
					{$t('common.clear')}
				</Button>
				<Button type="submit" size="sm" disabled={!formValid || submitting}>
					{#if submitting}
						<Spinner class="mr-1 h-3.5 w-3.5" />
					{:else}
						<Icon name="add-circle" class="mr-1 h-3.5 w-3.5" />
					{/if}
					{submitting ? $t('common.saving') : $t('memory.add')}
				</Button>
			</div>
		</form>
	</Card.Root>

	<!-- ===== Stored memory ============================================ -->
	<Card.Root class="gap-4 rounded-2xl p-5">
		<div class="flex flex-wrap items-center justify-between gap-3">
			<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
				<Icon name="diskette" class="text-muted-foreground h-4 w-4" />
				{$t('memory.title')}
				<span class="text-muted-foreground/70 ml-2 text-[11px] tabular-nums">
					{loaded ? `· ${entries.length}` : ''}
				</span>
			</div>
		</div>

		{#if !loaded}
			<div class="space-y-2 py-2">
				{#each { length: 3 } as _}
					<Skeleton class="h-14 w-full" />
				{/each}
			</div>
		{:else if entries.length === 0}
			<EmptyState icon="brain" title={$t('memory.none')} hint={$t('memory.inferredHint')} />
		{:else}
			{#if pending.length}
				<div class="space-y-2">
					<div
						class="text-muted-foreground/70 text-[11px] font-medium uppercase tracking-wider"
					>
						{$t('memory.state.proposed')}
					</div>
					{#each pending as entry (entry.id)}
						{@render entryRow(entry)}
					{/each}
					<p class="text-muted-foreground/80 flex items-start gap-1.5 text-[11px]">
						<Icon name="info-circle" class="mt-px h-3 w-3 shrink-0" />
						{$t('memory.inferredHint')}
					</p>
				</div>
				<Separator class="border-dashed" />
			{/if}

			<div class="space-y-2">
				<div class="text-muted-foreground/70 text-[11px] font-medium uppercase tracking-wider">
					{$t('memory.state.approved')}
				</div>
				{#each approved as entry (entry.id)}
					{@render entryRow(entry)}
				{:else}
					<p class="text-muted-foreground text-[11.5px]">{$t('memory.none')}</p>
				{/each}
			</div>
		{/if}
	</Card.Root>
</div>
