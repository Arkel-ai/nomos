<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import type { SolarIconName } from '$lib/icons/solar';
	import '../app.css';
	import NavigationMotion from '$lib/components/navigation-motion.svelte';
	import { onMount, untrack } from 'svelte';
	import type { Snippet } from 'svelte';
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { ModeWatcher, setMode, userPrefersMode } from 'mode-watcher';
	import { Toaster as SileoToaster } from '$lib/components/ui/sileo';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import * as Sidebar from '$lib/components/ui/sidebar';
	import * as Command from '$lib/components/ui/command';
	import * as Popover from '$lib/components/ui/popover';
	import {
		t,
		locale,
		setLocale,
		loadTranslations,
		defaultLocale,
		resolveLocale
	} from '$lib/i18n';
	import { authClient } from '$lib/auth/client';
	import { meStore, loadMe } from '$lib/stores/me';
	import { user } from '$lib/stores/user';
	import { listConversations, deleteConversation, type Conversation } from '$lib/api/conversations';
	import { conversation } from '$lib/stores/conversation';
	import ConfirmDialog from '$lib/components/confirm-dialog.svelte';
	import { sileo } from '$lib/components/ui/sileo';
	import NomosMark from '$lib/components/nomos-mark.svelte';
	import MobileNavTrigger from '$lib/components/mobile-nav-trigger.svelte';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import Tour from '$lib/components/tour.svelte';
	import { appTour, APP_TOUR_VERSION } from '$lib/app-tour';
	import { tourSeenVersion } from '$lib/stores/tour';

	let { children }: { children: Snippet } = $props();

	// ── Locale ───────────────────────────────────────────────────────────────
	// Nomos is French-first (docs/PLAN.md §11). `+layout.ts` boots translations
	// from the persisted user store, whose starter default is still 'en', so the
	// shell resolves the effective locale here: an explicit choice wins, and
	// anything else falls back to `fr`.
	const LANG_KEY = 'nomos.lang';

	function storedLocale(): string | null {
		try {
			return localStorage.getItem(LANG_KEY);
		} catch {
			return null;
		}
	}

	function applyLocale(next: string) {
		const value = resolveLocale(next);
		void loadTranslations(value);
		setLocale(value);
		// app.html ships lang="en" as a static default; keep the document in
		// sync so screen readers announce French with French pronunciation and
		// the browser offers the right hyphenation and spellcheck dictionary.
		if (typeof document !== 'undefined') document.documentElement.lang = value;
		try {
			localStorage.setItem(LANG_KEY, value);
		} catch {
			/* private mode: the language still applies for this session */
		}
		if ($user.settings) $user.settings.lang = value;
	}

	// ── Identity ─────────────────────────────────────────────────────────────
	const me = $derived($meStore.me);
	const isAdmin = $derived(!!me?.is_admin);
	const userName = $derived(me?.name || '');
	const userEmail = $derived(me?.email || '');
	const userInitial = $derived(userName.trim().charAt(0).toUpperCase() || 'N');

	// ── Routes ───────────────────────────────────────────────────────────────
	// The marketing landing and the auth pages are full-bleed. Everything
	// else — `/chat`, `/chat/[id]`, the dossiers, the library — renders inside
	// the shell.
	const bareRoute = $derived(
		$page.url.pathname === '/' ||
			$page.url.pathname.startsWith('/login') ||
			$page.url.pathname.startsWith('/accept-invite')
	);

	type RailItem = {
		titleKey: string;
		href: string;
		icon: SolarIconName;
		adminOnly?: boolean;
	};

	// The rail is an icon column, always collapsed: the inset frame keeps the
	// whole width for the work. Top to bottom: start a thread, search, history
	// (a popover — it lists threads, not a page), then the collections and admin.
	// Recherche (`/search`) and Compétences (`/skills`) are hidden for now; the
	// routes still exist, only the rail entries are pulled.
	const newThread: RailItem = { titleKey: 'nav.newConversation', href: '/chat', icon: 'pen-new-square' };

	const secondaryItems: RailItem[] = [
		{ titleKey: 'nav.dossiers', href: '/dossiers', icon: 'folder' },
		{ titleKey: 'admin.users', href: '/admin', icon: 'users-group-rounded', adminOnly: true }
	];

	// Held closed: ⌘B and the rail would otherwise expand it.
	let sidebarOpen = $state(false);

	const visibleSecondary = $derived(secondaryItems.filter((i) => !i.adminOnly || isAdmin));

	// `startsWith` rather than an equality test: SvelteKit types `pathname` as
	// the union of routes that exist today, and /chat is a sibling agent's page.
	const fullBleed = $derived($page.url.pathname.startsWith('/chat'));

	function isActive(href: string): boolean {
		const path = $page.url.pathname;
		return path === href || path.startsWith(href + '/');
	}

	// ── History ──────────────────────────────────────────────────────────────
	// The recent threads are fetched with the shell and refreshed on
	// navigation: a first exchange mints a thread id and a title, and the route
	// change is what tells us to pick them up. The rail's history popover and
	// the ⌘K palette both read them.
	let historyLoading = $state(false);
	let historyError = $state<string | null>(null);
	let recent = $state<Conversation[]>([]);

	async function loadRecent() {
		historyLoading = true;
		historyError = null;
		try {
			const page = await listConversations({ limit: 20 });
			recent = page.items ?? [];
		} catch {
			historyError = 'conversations.loadError';
		} finally {
			historyLoading = false;
		}
	}

	$effect(() => {
		const path = $page.url.pathname;
		if (path && !bareRoute) void loadRecent();
	});

	// The open thread is titled after the navigation that loaded the rail:
	// name it in place when the title lands, rather than refetching.
	$effect(() => {
		const open = $conversation.conversation;
		if (!open?.title) return;
		untrack(() => {
			if (recent.some((c) => c.id === open.id && c.title !== open.title)) {
				recent = recent.map((c) => (c.id === open.id ? { ...c, title: open.title } : c));
			}
		});
	});

	let historyOpen = $state(false);
	const onThread = $derived($page.url.pathname.startsWith('/chat/'));

	// Grouped the way people remember work: today, yesterday, this week, before.
	const historyGroups = $derived.by(() => {
		const startOfDay = new Date();
		startOfDay.setHours(0, 0, 0, 0);
		const today = startOfDay.getTime() / 1000;
		const yesterday = today - 86400;
		const week = today - 6 * 86400;
		const groups: { key: string; items: Conversation[] }[] = [
			{ key: 'conversations.groupToday', items: [] },
			{ key: 'conversations.groupYesterday', items: [] },
			{ key: 'conversations.groupWeek', items: [] },
			{ key: 'conversations.groupEarlier', items: [] }
		];
		for (const thread of recent) {
			const at = thread.updated_at || thread.created_at;
			const slot = at >= today ? 0 : at >= yesterday ? 1 : at >= week ? 2 : 3;
			groups[slot].items.push(thread);
		}
		return groups.filter((g) => g.items.length);
	});

	// Deleting a thread from the history: the popover closes, the dialog says
	// what goes, and a thread being viewed hands the canvas back to /chat.
	let threadToDelete = $state<Conversation | null>(null);
	let deleteThreadOpen = $state(false);

	function askDeleteThread(thread: Conversation) {
		threadToDelete = thread;
		historyOpen = false;
		deleteThreadOpen = true;
	}

	async function deleteThread() {
		const thread = threadToDelete;
		if (!thread) return;
		await deleteConversation(thread.id);
		recent = recent.filter((r) => r.id !== thread.id);
		sileo.success({ title: $t('conversations.deleted') });
		if ($page.url.pathname === `/chat/${thread.id}`) await goto('/chat');
	}

	function threadTime(thread: Conversation): string {
		const at = new Date((thread.updated_at || thread.created_at) * 1000);
		const sameDay = at.toDateString() === new Date().toDateString();
		return sameDay
			? at.toLocaleTimeString($locale || 'fr', { hour: '2-digit', minute: '2-digit' })
			: at.toLocaleDateString($locale || 'fr', { day: 'numeric', month: 'short' });
	}

	// ── Command palette (⌘K) ─────────────────────────────────────────────────
	let searchOpen = $state(false);

	// User menu — bound so navigating or launching the tour from it closes it.
	let userMenuOpen = $state(false);

	// Product tour: shown once per APP_TOUR_VERSION, replayable from the user
	// menu. Waits for /api/me so admin-only steps are decided before it opens.
	let tourOpen = $state(false);
	const tourSteps = $derived(
		appTour
			.filter((s) => !s.adminOnly || isAdmin)
			.map((s) => ({
				...s,
				title: $t(`tour.app.${s.key}.title`),
				body: $t(`tour.app.${s.key}.body`)
			}))
	);

	$effect(() => {
		if (!bareRoute && $meStore.loaded && $tourSeenVersion < APP_TOUR_VERSION)
			tourOpen = true;
	});

	function startTour() {
		userMenuOpen = false;
		tourOpen = true;
	}

	function handleGlobalKey(e: KeyboardEvent) {
		if (e.key === 'k' && (e.metaKey || e.ctrlKey)) {
			e.preventDefault();
			searchOpen = !searchOpen;
		}
	}

	function runCommand(href: string) {
		searchOpen = false;
		goto(href);
	}

	onMount(() => {
		applyLocale(storedLocale() ?? $user.settings?.lang ?? defaultLocale);
		// Load roles for nav gating; harmless (401) when signed out.
		loadMe();
	});

	async function signOut() {
		await authClient.signOut();
		meStore.set({ loading: false, loaded: true, me: null });
		await goto('/login');
	}
</script>

<svelte:head>
	<title>{$t('title')}</title>
</svelte:head>

<svelte:window onkeydown={handleGlobalKey} />

<ModeWatcher defaultMode="light" />
<NavigationMotion />
<SileoToaster position="top-right" theme="system" />

{#snippet kbdChip(label: string)}
	<kbd
		class="border-border bg-background inline-flex h-4 min-w-[16px] items-center justify-center rounded border px-1 font-mono text-[9px] font-medium"
	>
		{label}
	</kbd>
{/snippet}

<!-- One rail button: an icon in a 40px hit area, its name in a tooltip. -->
{#snippet railLink(item: RailItem)}
	{@const active = isActive(item.href)}
	<Tooltip.Root>
		<Tooltip.Trigger>
			{#snippet child({ props })}
				<a
					{...props}
					href={item.href}
					data-tour="nav:{item.href}"
					aria-label={$t(item.titleKey)}
					aria-current={active ? 'page' : undefined}
					class="rail-btn {active ? 'is-active' : ''}"
				>
					<Icon name={item.icon} {active} class="h-[17px] w-[17px]" />
					<span class="rail-label">{$t(item.titleKey)}</span>
				</a>
			{/snippet}
		</Tooltip.Trigger>
		<Tooltip.Content side="right" sideOffset={10}>{$t(item.titleKey)}</Tooltip.Content>
	</Tooltip.Root>
{/snippet}

<Command.Dialog
	bind:open={searchOpen}
	title={$t('common.search')}
	description={$t('nav.groupMain')}
>
	<Command.Input placeholder="{$t('common.search')}..." />
	<Command.List class="max-h-[440px]">
		<Command.Empty>
			<div class="flex flex-col items-center gap-2 py-8 text-center">
				<div
					class="bg-muted text-muted-foreground flex h-10 w-10 items-center justify-center rounded-xl"
				>
					<Icon name="magnifier" class="h-4 w-4" />
				</div>
				<div class="text-foreground text-ui font-medium">{$t('common.noResults')}</div>
			</div>
		</Command.Empty>

		<Command.Group heading={$t('nav.groupWorkspace')}>
			<Command.Item
				value="{$t(newThread.titleKey)} {newThread.href}"
				onSelect={() => runCommand(newThread.href)}
			>
				<div class="text-foreground flex h-6 w-6 items-center justify-center rounded-md">
					<Icon name={newThread.icon} class="h-3.5 w-3.5" />
				</div>
				<span class="font-medium">{$t(newThread.titleKey)}</span>
				<Command.Shortcut class="text-muted-foreground/60 font-mono text-2xs">
					{newThread.href}
				</Command.Shortcut>
			</Command.Item>
		</Command.Group>

		<Command.Group heading={$t('nav.groupLibrary')}>
			{#each visibleSecondary as item}
				<Command.Item
					value="{$t(item.titleKey)} {item.href}"
					onSelect={() => runCommand(item.href)}
				>
					<div class="text-foreground flex h-6 w-6 items-center justify-center rounded-md">
						<Icon name={item.icon} class="h-3.5 w-3.5" />
					</div>
					<span class="font-medium">{$t(item.titleKey)}</span>
					<Command.Shortcut class="text-muted-foreground/60 font-mono text-2xs">
						{item.href}
					</Command.Shortcut>
				</Command.Item>
			{/each}
		</Command.Group>

		{#if recent.length}
			<Command.Group heading={$t('conversations.recent')}>
				{#each recent.slice(0, 8) as thread}
					<Command.Item
						value="{thread.title || $t('conversations.untitled')} {thread.id}"
						onSelect={() => runCommand(`/chat/${thread.id}`)}
					>
						<div class="text-muted-foreground flex h-6 w-6 items-center justify-center rounded-md">
							<Icon name="history" class="h-3.5 w-3.5" />
						</div>
						<span class="truncate font-medium">
							{thread.title || $t('conversations.untitled')}
						</span>
					</Command.Item>
				{/each}
			</Command.Group>
		{/if}
	</Command.List>

	<div
		class="border-border bg-muted/30 text-muted-foreground flex items-center justify-between gap-3 border-t px-3 py-2 text-[10.5px]"
	>
		<div class="flex items-center gap-3">
			<span class="flex items-center gap-1">
				{@render kbdChip('↑')}
				{@render kbdChip('↓')}
				{$t('common.view')}
			</span>
			<span class="flex items-center gap-1">
				{@render kbdChip('↵')}
				{$t('common.open')}
			</span>
		</div>
		<span class="flex items-center gap-1">
			{@render kbdChip('esc')}
			{$t('common.close')}
		</span>
	</div>
</Command.Dialog>

<Tooltip.Provider delayDuration={150}>
	{#if bareRoute}
		<div class="route-content min-h-screen" data-sveltekit-preload-data="hover">
			{@render children()}
		</div>
	{:else}
		<Sidebar.Provider
			bind:open={sidebarOpen}
			onOpenChange={() => (sidebarOpen = false)}
			style="--sidebar-width-icon: 3.25rem;"
		>
			<Sidebar.Sidebar collapsible="icon" variant="inset" class="border-none">
				<Sidebar.Header class="flex items-center px-0 pt-3.5 pb-3">
					<a
						href="/chat"
						aria-label={$t('title')}
						class="flex h-8 w-8 items-center justify-center rounded-lg transition-opacity hover:opacity-80"
					>
						<NomosMark size={24} />
					</a>
				</Sidebar.Header>

				<Sidebar.Content class="items-center gap-0.5 px-0">
					<!-- The one primary action: an ink button, unlike the quiet icons. -->
					<Tooltip.Root>
						<Tooltip.Trigger>
							{#snippet child({ props })}
								<a
									{...props}
									href={newThread.href}
									data-tour="nav:{newThread.href}"
									aria-label={$t(newThread.titleKey)}
									class="rail-btn is-primary mb-1.5 {$page.url.pathname === '/chat' ? 'is-active' : ''}"
								>
									<Icon name={newThread.icon} class="h-[17px] w-[17px]" />
									<span class="rail-label">{$t(newThread.titleKey)}</span>
								</a>
							{/snippet}
						</Tooltip.Trigger>
						<Tooltip.Content side="right" sideOffset={10}>
							{$t(newThread.titleKey)}
						</Tooltip.Content>
					</Tooltip.Root>

					<Tooltip.Root>
						<Tooltip.Trigger>
							{#snippet child({ props })}
								<button
									{...props}
									type="button"
									onclick={() => (searchOpen = true)}
									data-tour="search"
									aria-label="{$t('common.search')} (⌘K)"
									class="rail-btn"
								>
									<Icon name="magnifier" class="h-[17px] w-[17px]" />
									<span class="rail-label">{$t('common.search')}</span>
								</button>
							{/snippet}
						</Tooltip.Trigger>
						<Tooltip.Content side="right" sideOffset={10}>
							<span class="flex items-center gap-2">
								{$t('common.search')}
								<span class="opacity-60">⌘K</span>
							</span>
						</Tooltip.Content>
					</Tooltip.Root>

					<!-- History: the threads, one click away without leaving the page. -->
					<Popover.Root bind:open={historyOpen}>
						<Popover.Trigger>
							{#snippet child({ props })}
								<button
									{...props}
									type="button"
									data-tour="history"
									aria-label={$t('nav.history')}
									class="rail-btn {onThread || historyOpen ? 'is-active' : ''}"
								>
									<Icon name="history" active={onThread || historyOpen} class="h-[17px] w-[17px]" />
									<span class="rail-label">{$t('nav.history')}</span>
								</button>
							{/snippet}
						</Popover.Trigger>
						<Popover.Content
							side="right"
							align="start"
							sideOffset={12}
							class="w-80 overflow-hidden rounded-2xl p-0 shadow-xl"
						>
							<div class="flex items-center gap-2 px-4 pt-3.5 pb-2">
								<span class="text-foreground text-ui font-semibold">{$t('nav.history')}</span>
								{#if recent.length}
									<span class="text-muted-foreground font-mono text-2xs tabular-nums">
										{recent.length}
									</span>
								{/if}
								<a
									href="/chat"
									onclick={() => (historyOpen = false)}
									class="text-muted-foreground hover:text-foreground hover:bg-muted ml-auto inline-flex h-7 items-center gap-1.5 rounded-lg px-2 text-xs transition-colors"
								>
									<Icon name="pen-new-square" class="h-3.5 w-3.5" />
									{$t('nav.newConversation')}
								</a>
							</div>
							<div class="max-h-[min(70vh,520px)] overflow-y-auto px-2 pb-2">
								{#if historyLoading && !recent.length}
									<div class="space-y-2 p-2">
										{#each [72, 56, 64, 48] as width}
											<Skeleton class="h-4 rounded-md" style="width: {width}%" />
										{/each}
									</div>
								{:else if historyError}
									<p class="text-muted-foreground px-2 py-3 text-xs">{$t(historyError)}</p>
								{:else if !recent.length}
									<div class="flex flex-col items-center gap-2 px-4 py-8 text-center">
										<Icon name="chat-round-dots" class="text-muted-foreground h-5 w-5" />
										<p class="text-muted-foreground text-xs">{$t('conversations.none')}</p>
									</div>
								{:else}
									{#each historyGroups as group (group.key)}
										<div
											class="text-muted-foreground/80 px-2 pt-3 pb-1 text-[10px] font-medium tracking-[0.1em] uppercase"
										>
											{$t(group.key)}
										</div>
										{#each group.items as thread (thread.id)}
											{@const current = $page.url.pathname === `/chat/${thread.id}`}
											<div
												class="group/thread relative flex items-center rounded-lg transition-colors {current
													? 'bg-muted'
													: 'hover:bg-muted/60'}"
											>
												<a
													href="/chat/{thread.id}"
													onclick={() => (historyOpen = false)}
													aria-current={current ? 'page' : undefined}
													class="flex min-w-0 flex-1 items-center gap-3 py-1.5 pr-9 pl-2 {current
														? 'text-foreground'
														: 'text-muted-foreground group-hover/thread:text-foreground'}"
												>
													<span class="min-w-0 flex-1 truncate text-ui">
														{thread.title || $t('conversations.untitled')}
													</span>
													<span
														class="text-muted-foreground/70 shrink-0 font-mono text-[10px] tabular-nums transition-opacity group-hover/thread:opacity-0"
													>
														{threadTime(thread)}
													</span>
												</a>
												<button
													type="button"
													onclick={() => askDeleteThread(thread)}
													aria-label={$t('conversations.delete')}
													title={$t('conversations.delete')}
													class="text-muted-foreground hover:text-destructive hover:bg-destructive/10 absolute right-1 flex h-7 w-7 cursor-pointer items-center justify-center rounded-md opacity-0 transition-opacity group-hover/thread:opacity-100 focus-visible:opacity-100"
												>
													<Icon name="trash-bin-trash" class="h-3.5 w-3.5" />
												</button>
											</div>
										{/each}
									{/each}
								{/if}
							</div>
						</Popover.Content>
					</Popover.Root>

					<div class="bg-sidebar-border my-1.5 h-px w-4" aria-hidden="true"></div>

					{#each visibleSecondary as item (item.href)}
						{@render railLink(item)}
					{/each}
				</Sidebar.Content>

				<Sidebar.Footer class="items-center gap-2 px-0 pb-3.5">
					<Popover.Root bind:open={userMenuOpen}>
						<Popover.Trigger>
							{#snippet child({ props })}
								<button
									{...props}
									type="button"
									data-tour="user-menu"
									aria-label={userName || $t('nav.account')}
									class="ring-sidebar-border hover:ring-foreground/25 flex h-8 w-8 cursor-pointer items-center justify-center rounded-full ring-1 transition-shadow"
								>
									<span
										class="bg-muted text-foreground flex h-7 w-7 items-center justify-center rounded-full text-xs font-semibold"
									>
										{userInitial}
									</span>
								</button>
							{/snippet}
						</Popover.Trigger>

						<Popover.Content
							side="right"
							align="end"
							sideOffset={12}
							class="w-64 overflow-hidden rounded-2xl p-0 shadow-xl"
						>
							<div class="flex items-center gap-3 px-3.5 py-3.5">
								<span
									class="bg-primary text-primary-foreground flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-sm font-semibold"
								>
									{userInitial}
								</span>
								<div class="min-w-0 flex-1 leading-tight">
									<div class="truncate text-ui font-semibold">{userName}</div>
									<div class="text-muted-foreground truncate text-2xs">{userEmail}</div>
								</div>
							</div>

							<div class="bg-border h-px"></div>

							<div class="space-y-3 p-2">
								<div>
									<div
										class="text-muted-foreground mb-1.5 px-1 text-[10px] font-medium tracking-[0.1em] uppercase"
									>
										{$t('nav.theme')}
									</div>
									<div class="bg-muted/60 grid grid-cols-3 gap-1 rounded-lg p-1">
										{#each [{ m: 'light', Icon: 'sun' as const, label: $t('theme.light') }, { m: 'dark', Icon: 'moon' as const, label: $t('theme.dark') }, { m: 'system', Icon: 'monitor' as const, label: $t('theme.system') }] as opt}
											{@const active = userPrefersMode.current === opt.m}
											<button
												type="button"
												onclick={() => setMode(opt.m as 'light' | 'dark' | 'system')}
												class="flex cursor-pointer flex-col items-center justify-center gap-1 rounded-md px-2 py-2 text-[10.5px] font-medium transition-all duration-150 {active
													? 'bg-card text-foreground shadow-sm'
													: 'text-muted-foreground hover:text-foreground'}"
											>
												<Icon name={opt.Icon} {active} class="h-3.5 w-3.5" />
												{opt.label}
											</button>
										{/each}
									</div>
								</div>

								<div>
									<div
										class="text-muted-foreground mb-1.5 px-1 text-[10px] font-medium tracking-[0.1em] uppercase"
									>
										{$t('nav.language')}
									</div>
									<div class="bg-muted/60 grid grid-cols-2 gap-1 rounded-lg p-1">
										{#each [{ v: 'fr', label: $t('nav.fr'), code: 'FR' }, { v: 'en', label: $t('nav.en'), code: 'EN' }] as opt}
											{@const active = $locale === opt.v}
											<button
												type="button"
												onclick={() => applyLocale(opt.v)}
												class="flex cursor-pointer items-center justify-center gap-1.5 rounded-md px-2 py-1.5 text-xs font-medium transition-all duration-150 {active
													? 'bg-card text-foreground shadow-sm'
													: 'text-muted-foreground hover:text-foreground'}"
											>
												<span class="font-mono text-[10px] font-semibold opacity-60">{opt.code}</span>
												<span>{opt.label}</span>
											</button>
										{/each}
									</div>
								</div>
							</div>

							<div class="bg-border h-px"></div>

							<div class="p-1">
								<a
									href="/settings"
									onclick={() => (userMenuOpen = false)}
									class="hover:bg-muted flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-ui font-medium transition-colors"
								>
									<Icon name="settings" class="h-4 w-4" />
									{$t('nav.settings')}
								</a>
								<button
									type="button"
									onclick={startTour}
									class="hover:bg-muted flex w-full cursor-pointer items-center gap-2 rounded-md px-2.5 py-2 text-ui font-medium transition-colors"
								>
									<Icon name="compass" class="h-4 w-4" />
									{$t('tour.replay')}
								</button>
								<button
									type="button"
									onclick={signOut}
									class="text-destructive hover:bg-destructive/10 flex w-full cursor-pointer items-center gap-2 rounded-md px-2.5 py-2 text-ui font-medium transition-colors"
								>
									<Icon name="logout" class="h-4 w-4" />
									{$t('auth.signOut')}
								</button>
							</div>

							<a
								href="https://arkel.ai"
								target="_blank"
								rel="noopener noreferrer"
								class="border-border text-muted-foreground/70 hover:text-muted-foreground block border-t px-3.5 py-2 text-center text-[10px] tracking-wide transition-colors"
							>
								{$t('common.poweredBy')}
							</a>
						</Popover.Content>
					</Popover.Root>
				</Sidebar.Footer>
			</Sidebar.Sidebar>

			<MobileNavTrigger />
			<Sidebar.Inset class="route-content shadow-shell border-border/60 overflow-hidden border">
				<!-- The thread owns its whole canvas (composer pinned to the bottom,
				     messages scrolling above it), so /chat is full-bleed. Every other
				     page keeps the shell padding. -->
				<div
					class="surface-noise flex-1 overflow-auto {fullBleed ? '' : 'px-6 py-8 sm:px-10'}"
					data-sveltekit-preload-data="hover"
				>
					{@render children()}
				</div>
			</Sidebar.Inset>
		</Sidebar.Provider>
	{/if}
</Tooltip.Provider>

<ConfirmDialog
	bind:open={deleteThreadOpen}
	title={$t('conversations.deleteTitle')}
	description={$t('conversations.deleteBody')}
	details={[$t('conversations.deleteMessages'), $t('conversations.deleteKeepsRuns')]}
	confirmLabel={$t('conversations.delete')}
	onconfirm={deleteThread}
/>

<Tour
	bind:open={tourOpen}
	steps={tourSteps}
	onclose={() => ($tourSeenVersion = APP_TOUR_VERSION)}
/>
