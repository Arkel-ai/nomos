<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import * as Popover from '$lib/components/ui/popover';
	import { Calendar } from '$lib/components/ui/calendar';
	import { CalendarDate, type DateValue } from '@internationalized/date';
	import { t, locale } from '$lib/i18n';

	/**
	 * A date as a quiet field: the formatted date at rest ("15 janv. 2025"),
	 * a calendar in a popover when engaged. Speaks unix seconds at a UTC
	 * midnight — the project's timestamp convention — so the host never
	 * handles calendar types. Replaces the native date input, whose look and
	 * height differ per browser.
	 */
	let {
		value = null,
		disabled = false,
		label,
		class: className = '',
		onchange
	}: {
		value?: number | null;
		disabled?: boolean;
		label?: string;
		class?: string;
		onchange?: (seconds: number | null) => void;
	} = $props();

	let open = $state(false);

	function toCalendar(seconds: number | null): DateValue | undefined {
		if (!seconds) return undefined;
		const d = new Date(seconds * 1000);
		return new CalendarDate(d.getUTCFullYear(), d.getUTCMonth() + 1, d.getUTCDate());
	}

	function toSeconds(date: DateValue | undefined): number | null {
		return date ? Date.UTC(date.year, date.month - 1, date.day) / 1000 : null;
	}

	const picked = $derived(toCalendar(value));
	const shown = $derived(
		value
			? new Date(value * 1000).toLocaleDateString($locale || 'fr', {
					day: 'numeric',
					month: 'short',
					year: 'numeric',
					timeZone: 'UTC'
				})
			: ''
	);

	function pick(next: DateValue | undefined) {
		open = false;
		const seconds = toSeconds(next);
		if (seconds !== value) onchange?.(seconds);
	}
</script>

<Popover.Root bind:open>
	<Popover.Trigger {disabled}>
		{#snippet child({ props })}
			<button
				{...props}
				type="button"
				aria-label={label}
				class="hover:border-border focus-visible:border-ring data-[state=open]:border-border data-[state=open]:bg-card group flex h-8 w-full cursor-pointer items-center gap-2 rounded-lg border border-transparent px-2 text-left text-ui tabular-nums transition-colors disabled:cursor-not-allowed disabled:opacity-50 {className}"
			>
				<Icon
					name="calendar"
					class="text-muted-foreground group-hover:text-foreground h-4 w-4 shrink-0 transition-colors"
				/>
				{#if shown}
					<span class="text-foreground truncate">{shown}</span>
				{:else}
					<span class="text-muted-foreground/70 truncate">{$t('common.noDate')}</span>
				{/if}
			</button>
		{/snippet}
	</Popover.Trigger>
	<Popover.Content class="w-auto overflow-hidden rounded-2xl p-0" align="start">
		<Calendar
			type="single"
			value={picked}
			captionLayout="dropdown"
			locale={$locale || 'fr'}
			onValueChange={(next) => pick(next)}
		/>
		{#if value}
			<div class="border-border border-t p-1.5">
				<button
					type="button"
					onclick={() => pick(undefined)}
					class="text-muted-foreground hover:bg-muted hover:text-foreground flex w-full cursor-pointer items-center justify-center gap-1.5 rounded-lg py-1.5 text-xs transition-colors"
				>
					<Icon name="close-circle" class="h-3.5 w-3.5" />
					{$t('common.clearDate')}
				</button>
			</div>
		{/if}
	</Popover.Content>
</Popover.Root>
