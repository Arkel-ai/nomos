<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import * as Popover from '$lib/components/ui/popover';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { sileo } from '$lib/components/ui/sileo';
	import { Composer, fileStaged, type Attachment, type ComposerPayload } from '$lib/components/composer';
	import { t } from '$lib/i18n';
	import { meStore, loadMe } from '$lib/stores/me';
	import {
		collection,
		dossiers,
		loadDossiers,
		loadDossier,
		resetCurrentDossier
	} from '$lib/stores/dossier';
	import { createConversation, postMessage } from '$lib/api/conversations';
	import { createDossier } from '$lib/api/dossiers';
	import { intakeErrorKey } from '$lib/api/conversations';

	/**
	 * The home (docs/PLAN.md §11). A calm single column: the collection pill,
	 * the greeting, the two-tone headline and the composer. Files are staged
	 * in the composer; submitting files them, creates the conversation WITH
	 * its first turn, then navigates — the lawyer lands on their own message,
	 * never on an empty thread.
	 */

	// sveltekit-i18n types the default payload as empty, so an interpolated key
	// (`{{count}}`, `{{name}}`) needs a widened signature until the shared
	// config declares one.
	const tx = $derived($t as unknown as (key: string, payload?: Record<string, unknown>) => string);

	let value = $state('');
	let attachments = $state<Attachment[]>([]);
	let skill = $state<string | null>(null);
	let busy = $state(false);
	let collectionOpen = $state(false);

	const firstName = $derived(($meStore.me?.name ?? '').trim().split(/\s+/)[0] ?? '');
	const greeting = $derived(
		firstName ? tx('chat.greeting', { name: firstName }) : $t('chat.greetingAnon')
	);
	const collectionCount = $derived($collection?.count ?? 0);

	// The headline arrives word by word; its second half is the italic accent.
	const headline = $derived([
		...$t('chat.headlineAccent').split(' ').map((word: string) => ({ word, accent: false })),
		...$t('chat.headlineRest').split(' ').map((word: string) => ({ word, accent: true }))
	]);

	onMount(() => {
		// A new thread starts unbound: the dossier of the last thread or dossier
		// page opened still sits in the shared store, and must not carry over.
		resetCurrentDossier();
		loadMe();
		loadDossiers().catch(() => {
			// The dossier list is a convenience here: the home still composes.
		});
	});

	async function bindDossier(id: string) {
		collectionOpen = false;
		await loadDossier(id);
	}

	function startNewDossier() {
		collectionOpen = false;
		resetCurrentDossier();
	}

	/**
	 * One submit does the whole turn. Staged files are filed as pièces —
	 * Nomos reads nothing else — into the bound dossier, or into one created
	 * for them, which the API names from this turn as it titles the thread.
	 * The conversation is then created WITH its first turn, and that turn
	 * waits for the pièces to be read before it is answered.
	 */
	async function onsubmit(payload: ComposerPayload) {
		if (busy) return;
		busy = true;
		try {
			let dossierId = $collection?.id ?? null;
			let files = payload.attachments;
			if (files.some((a) => a.file)) {
				if (!dossierId) {
					dossierId = (await createDossier({ title: $t('chat.collectionNew'), auto_title: true }))
						.id;
					// Bound before the upload: a retry files into this dossier, never a second one.
					await loadDossier(dossierId);
					loadDossiers(true).catch(() => {});
				}
				const filed = await fileStaged(dossierId, files);
				// The pièces replace the staged files, so a retry re-sends the turn, not the uploads.
				attachments = files = filed.attachments;
				if (filed.rejected) sileo.error($t('errors.unsupportedType'));
			}
			const message = {
				content: payload.text,
				attachment_ids: files.map((a) => a.id),
				skill_slugs: payload.skill ? [payload.skill] : [],
				dossier_id: dossierId
			};
			const created = await createConversation({
				dossier_id: dossierId,
				message
			});
			// A build that ignores the inline first message still gets the turn:
			// send it once against the thread we just created.
			if (!created.messages?.length) await postMessage(created.id, message);
			await goto(`/chat/${created.id}`);
		} catch (err) {
			busy = false;
			const intakeKey = intakeErrorKey(err);
			sileo.error(
				intakeKey
					? $t(intakeKey)
					: ((err as { message?: string })?.message ?? $t('chat.createError'))
			);
		}
	}
</script>

<div class="relative flex min-h-full flex-col px-5 py-4">
	<!-- A faint aura of the one colour, behind the composer only. -->
	<div
		class="pointer-events-none absolute inset-x-0 top-[18%] mx-auto h-[420px] max-w-3xl rounded-full opacity-70 blur-3xl"
		style="background: radial-gradient(closest-side, color-mix(in oklab, var(--brand) 9%, transparent), transparent)"
		aria-hidden="true"
	></div>

	<!-- Collection selector — how a thread binds to a dossier -->
	<div class="rise-in relative z-10 shrink-0">
		<Popover.Root bind:open={collectionOpen}>
			<Popover.Trigger>
				{#snippet child({ props })}
					<button
						{...props}
						type="button"
						class="bg-muted/70 text-foreground hover:bg-muted flex h-8 cursor-pointer items-center gap-2 rounded-lg pr-2 pl-2.5 text-ui font-medium transition-colors"
					>
						<Icon
							name="folder"
							active={!!$collection}
							class="h-4 w-4 {$collection ? 'text-brand' : 'text-muted-foreground'}"
						/>
						<span class="max-w-[240px] truncate">
							{$collection?.title ?? $t('chat.collectionNew')}
						</span>
						<span
							class="bg-card text-muted-foreground inline-flex h-5 min-w-5 items-center justify-center rounded-md px-1.5 font-mono text-[10px] tabular-nums"
						>
							{collectionCount}
						</span>
						<Icon name="alt-arrow-down" class="text-muted-foreground h-3.5 w-3.5" />
					</button>
				{/snippet}
			</Popover.Trigger>
			<Popover.Content align="start" class="w-80 rounded-2xl p-1.5">
				<button
					type="button"
					onclick={startNewDossier}
					class="hover:bg-accent flex w-full cursor-pointer items-center gap-2.5 rounded-xl px-2.5 py-2 text-left transition-colors"
				>
					<Icon
						name="add-circle"
						active={!$collection}
						class="h-4 w-4 shrink-0 {$collection ? 'text-muted-foreground' : 'text-brand'}"
					/>
					<span class="text-foreground min-w-0 flex-1 truncate text-ui font-medium">
						{$t('chat.collectionNew')}
					</span>
					{#if !$collection}
						<Icon name="check-circle" active class="text-brand h-4 w-4 shrink-0" />
					{/if}
				</button>
				<div class="bg-border mx-2 my-1 h-px"></div>
				{#if $dossiers.loading}
					<div class="space-y-2 p-2">
						{#each [70, 54, 62] as width}
							<Skeleton class="h-4 rounded-md" style="width: {width}%" />
						{/each}
					</div>
				{:else if $dossiers.items.length === 0}
					<div class="text-muted-foreground px-3 py-6 text-center text-xs">
						{$t('chat.collectionEmpty')}
					</div>
				{:else}
					{#each $dossiers.items as item (item.id)}
						{@const current = $collection?.id === item.id}
						<button
							type="button"
							onclick={() => bindDossier(item.id)}
							class="hover:bg-accent flex w-full cursor-pointer items-center gap-2.5 rounded-xl px-2.5 py-2 text-left transition-colors"
						>
							<Icon
								name="folder"
								active={current}
								class="h-4 w-4 shrink-0 {current ? 'text-brand' : 'text-muted-foreground'}"
							/>
							<span class="min-w-0 flex-1">
								<span class="text-foreground block truncate text-ui font-medium">
									{item.title}
								</span>
								<span class="text-muted-foreground block truncate font-mono text-2xs">
									{item.client_reference}
								</span>
							</span>
							{#if current}
								<Icon name="check-circle" active class="text-brand h-4 w-4 shrink-0" />
							{/if}
						</button>
					{/each}
				{/if}
			</Popover.Content>
		</Popover.Root>
	</div>

	<!-- The canvas: greeting, headline, composer -->
	<div class="relative flex flex-1 items-center justify-center py-12">
		<div class="w-full max-w-3xl space-y-9">
			<div class="space-y-4 text-center">
				<p class="rise-in text-muted-foreground text-sm">{greeting}</p>
				<h1 class="display text-foreground text-[44px] sm:text-[60px]">
					{#each headline as part, i}
						{#if i > 0}{' '}{/if}<span
							class="word-in inline-block"
							style="animation-delay: {80 + i * 70}ms"
							>{#if part.accent}<em>{part.word}</em>{:else}{part.word}{/if}</span
						>
					{/each}
				</h1>
			</div>

			<div class="rise-in" style="animation-delay: 380ms">
				<Composer
					bind:value
					bind:attachments
					bind:skill
					collectionTitle={$collection?.title ?? null}
					{busy}
					live={busy}
					autofocus
					{onsubmit}
				/>
				<p class="text-muted-foreground mt-3 text-center text-xs leading-relaxed">
					{$t('chat.agent.intro')}
				</p>
				<p class="text-muted-foreground/80 mt-2.5 text-center text-2xs">
					{$t('chat.composer.enterToSend')} · {$t('chat.composer.skillsHint')}
				</p>
			</div>
		</div>
	</div>
</div>
