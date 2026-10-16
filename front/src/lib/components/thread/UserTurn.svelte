<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import type { SolarIconName } from '$lib/icons/solar';
	import { t, locale } from '$lib/i18n';
	import type { Message } from '$lib/api/conversations';

	/** A user turn: the instruction, plus the pièces it put in context. */

	let { message }: { message: Message } = $props();

	/** A long drop shows its first pièces; the rest open on demand. */
	const VISIBLE = 3;
	const attachments = $derived(message.attachments ?? []);
	let showAll = $state(false);
	const shown = $derived(showAll ? attachments : attachments.slice(0, VISIBLE));
	const hidden = $derived(attachments.length - VISIBLE);
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);

	function fileIcon(name = ''): SolarIconName {
		if (/\.(xlsx?|csv)$/i.test(name)) return 'bill-list';
		if (/\.(docx?|odt|rtf)$/i.test(name)) return 'document-text';
		return 'file-text';
	}
</script>

<div class="rise-in flex justify-end">
	<div class="flex max-w-[80%] min-w-0 flex-col items-end">
		<div
			class="bg-muted text-foreground rounded-[20px] rounded-br-md px-4 py-2.5 text-sm leading-relaxed whitespace-pre-wrap"
		>
			{message.content}
		</div>

		{#if attachments.length}
			<div class="mt-2 flex flex-wrap justify-end gap-1.5">
				{#each shown as attachment, i (`${attachment.id}:${i}`)}
					<span
						class="inline-flex h-7 max-w-[160px] items-center gap-1.5 rounded-lg px-2 text-2xs transition-colors bg-muted/60 text-muted-foreground {i >= VISIBLE ? 'rise-in' : ''}"
						style={i >= VISIBLE ? `animation-delay: ${Math.min((i - VISIBLE) * 20, 280)}ms` : undefined}
						title={attachment.name || attachment.id}
					>
						<Icon name={fileIcon(attachment.name)} class="h-3.5 w-3.5 shrink-0" />
						<span class="truncate">{attachment.name || attachment.id}</span>
					</span>
				{/each}
				{#if hidden > 0}
					<button
						type="button"
						class="inline-flex h-7 items-center gap-1 rounded-lg px-2 text-2xs font-medium text-muted-foreground transition-colors duration-200 hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand ring-offset-background"
						aria-expanded={showAll}
						onclick={() => (showAll = !showAll)}
					>
						{#if showAll}
							{$t('chat.turnAttachments.collapse')}
						{:else}
							{tx('chat.turnAttachments.more', { count: new Intl.NumberFormat($locale || 'fr').format(hidden) })}
						{/if}
					</button>
				{/if}
			</div>
		{/if}
	</div>
</div>
