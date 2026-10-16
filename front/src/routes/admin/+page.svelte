<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// =====================================================================
	// ADMIN USER PANEL — admin-only.
	// Two sections in the list-mockup rhythm: 'users-group-rounded' (change role) and
	// Invitations (create / revoke). The nav entry that links here is shown
	// only to admins (root +layout.svelte), and this page redirects away if
	// the current user isn't an admin. The real gate is the Go API: every
	// endpoint here is behind RequireAuth + RequirePermission("admin", …).
	// =====================================================================
	import { onMount } from 'svelte';
	import { fly } from 'svelte/transition';
	import { cubicOut } from 'svelte/easing';
	import { goto } from '$app/navigation';
	import { get } from 'svelte/store';
	import { t } from '$lib/i18n';
	import { meStore, loadMe } from '$lib/stores/me';
	import { listUsers, setUserRole, type AdminUser } from '$lib/api/admin';
	import {
		listInvitations,
		createInvitation,
		revokeInvitation,
		type Invitation
	} from '$lib/api/invitations';
	import * as Card from '$lib/components/ui/card';
	import * as Table from '$lib/components/ui/table';
	import * as Select from '$lib/components/ui/select';
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { Badge } from '$lib/components/ui/badge';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Button } from '$lib/components/ui/button';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import EmptyState from '$lib/components/empty-state.svelte';

	const ROLES = ['admin', 'user'];

	let authorized = $state<'checking' | 'yes' | 'no'>('checking');
	let loaded = $state(false);
	let users = $state<AdminUser[]>([]);
	let invitations = $state<Invitation[]>([]);
	let pageError = $state('');

	onMount(async () => {
		let me = get(meStore).me;
		if (!get(meStore).loaded) me = await loadMe();
		if (!me?.is_admin) {
			authorized = 'no';
			await goto('/');
			return;
		}
		authorized = 'yes';
		await refresh();
	});

	async function refresh() {
		pageError = '';
		const [u, i] = await Promise.all([listUsers(), listInvitations()]);
		if (u.status === 200) users = u.data.users;
		else pageError = u.data?.detail ?? $t('admin.loadError');
		if (i.status === 200) invitations = i.data.invitations;
		loaded = true;
	}

	function roleOf(u: AdminUser): string {
		return u.roles?.includes('admin') ? 'admin' : u.roles?.length ? u.roles[0] : '';
	}

	let savingUser = $state<string | null>(null);

	async function changeRole(u: AdminUser, role: string) {
		if (role === roleOf(u)) return;
		savingUser = u.id;
		pageError = '';
		const res = await setUserRole(u.id, role);
		if (res.status === 200) {
			users = users.map((x) => (x.id === u.id ? { ...x, roles: [role] } : x));
		} else {
			pageError = res.data?.detail ?? $t('admin.roleError');
		}
		savingUser = null;
	}

	// ---- invite dialog --------------------------------------------------
	let inviteOpen = $state(false);
	let inviteEmail = $state('');
	let inviteRole = $state('user');
	let inviting = $state(false);
	let inviteError = $state('');
	let createdLink = $state('');
	let linkCopied = $state(false);

	const inviteValid = $derived(/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(inviteEmail.trim()));

	function openInvite() {
		inviteEmail = '';
		inviteRole = 'user';
		inviteError = '';
		createdLink = '';
		linkCopied = false;
		inviteOpen = true;
	}

	async function submitInvite() {
		if (!inviteValid || inviting) return;
		inviting = true;
		inviteError = '';
		const res = await createInvitation(inviteEmail.trim(), inviteRole);
		if (res.status === 200) {
			createdLink = res.data.link;
			await refresh();
		} else {
			inviteError = res.data?.detail ?? $t('admin.inviteError');
		}
		inviting = false;
	}

	async function copyLink() {
		try {
			await navigator.clipboard.writeText(createdLink);
			linkCopied = true;
			setTimeout(() => (linkCopied = false), 1400);
		} catch {
			/* clipboard unavailable */
		}
	}

	async function revoke(inv: Invitation) {
		const res = await revokeInvitation(inv.id);
		if (res.status === 200) await refresh();
		else pageError = res.data?.detail ?? $t('admin.revokeError');
	}

	const STATUS_VARIANT: Record<string, 'default' | 'secondary' | 'outline'> = {
		pending: 'secondary',
		accepted: 'default',
		revoked: 'outline'
	};

	function fmtDate(unix: number): string {
		if (!unix) return '—';
		return new Date(unix * 1000).toLocaleDateString();
	}
</script>

<svelte:head>
	<title>{$t('admin.title')}</title>
</svelte:head>

{#if authorized !== 'yes'}
	<div class="space-y-2 py-2">
		{#each { length: 4 } as _}
			<Skeleton class="h-8 w-full" />
		{/each}
	</div>
{:else}
	<div class="mx-auto w-full max-w-6xl space-y-4" in:fly={{ y: 12, duration: 400, easing: cubicOut }}>
		<header class="space-y-2 pb-2">
			<h1 class="display text-foreground text-[40px]">{$t('admin.title')}</h1>
			<p class="text-muted-foreground text-sm">{$t('admin.subtitle')}</p>
		</header>
		{#if pageError}
			<div class="bg-destructive/10 text-destructive rounded-lg px-3 py-2 text-[12.5px]">
				{pageError}
			</div>
		{/if}

		<!-- ===== Users ===================================================== -->
		<Card.Root class="gap-3 rounded-2xl p-5">
			<div class="flex flex-wrap items-center justify-between gap-3">
				<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
					<Icon name="users-group-rounded" class="text-muted-foreground h-4 w-4" />
					{$t('admin.users')}
					<span class="ml-2 text-[11px] tabular-nums text-muted-foreground/70">
						{loaded ? `· ${users.length}` : ''}
					</span>
				</div>
				<Button size="sm" onclick={openInvite}>
					<Icon name="user-plus-rounded" class="mr-1 h-3.5 w-3.5" />
					{$t('admin.invite')}
				</Button>
			</div>

			{#if !loaded}
				<div class="space-y-2 py-2">
					{#each { length: 4 } as _}
						<Skeleton class="h-8 w-full" />
					{/each}
				</div>
			{:else}
				<Table.Root class="[&_thead]:bg-transparent">
					<Table.Header class="bg-transparent">
						<Table.Row class="border-0 hover:bg-transparent">
							<Table.Head class="text-[10px] uppercase tracking-wider text-muted-foreground">{$t('login.name')}</Table.Head>
							<Table.Head class="text-[10px] uppercase tracking-wider text-muted-foreground">{$t('auth.email')}</Table.Head>
							<Table.Head class="w-[160px] text-[10px] uppercase tracking-wider text-muted-foreground">{$t('admin.role')}</Table.Head>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{#each users as u (u.id)}
							<Table.Row class="border-dashed border-border/80 transition-colors hover:bg-muted/40">
								<Table.Cell class="text-[12.5px] font-semibold">
									{u.name || '—'}
								</Table.Cell>
								<Table.Cell class="text-[12.5px] text-muted-foreground">{u.email}</Table.Cell>
								<Table.Cell>
									<Select.Root
										type="single"
										value={roleOf(u)}
										onValueChange={(v) => v && changeRole(u, v)}
										disabled={savingUser === u.id}
									>
										<Select.Trigger class="h-8 w-[140px] text-[12.5px] capitalize">
											{roleOf(u) || $t('admin.noRole')}
										</Select.Trigger>
										<Select.Content align="end">
											{#each ROLES as r}
												<Select.Item value={r} class="capitalize">{$t(`admin.roleName.${r}`)}</Select.Item>
											{/each}
										</Select.Content>
									</Select.Root>
								</Table.Cell>
							</Table.Row>
						{:else}
							<Table.Row class="hover:bg-transparent">
								<Table.Cell colspan={3} class="p-0">
									<EmptyState icon="users-group-rounded" title={$t('common.noResults')} />
								</Table.Cell>
							</Table.Row>
						{/each}
					</Table.Body>
				</Table.Root>
			{/if}
		</Card.Root>

		<!-- ===== Invitations ============================================== -->
		<Card.Root class="gap-3 rounded-2xl p-5">
			<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
				<Icon name="letter" class="text-muted-foreground h-4 w-4" />
				{$t('admin.invitations')}
				<span class="ml-2 text-[11px] tabular-nums text-muted-foreground/70">
					{loaded ? `· ${invitations.length}` : ''}
				</span>
			</div>

			{#if !loaded}
				<div class="space-y-2 py-2">
					{#each { length: 3 } as _}
						<Skeleton class="h-8 w-full" />
					{/each}
				</div>
			{:else}
				<Table.Root class="[&_thead]:bg-transparent">
					<Table.Header class="bg-transparent">
						<Table.Row class="border-0 hover:bg-transparent">
							<Table.Head class="text-[10px] uppercase tracking-wider text-muted-foreground">{$t('auth.email')}</Table.Head>
							<Table.Head class="text-[10px] uppercase tracking-wider text-muted-foreground">{$t('admin.role')}</Table.Head>
							<Table.Head class="text-[10px] uppercase tracking-wider text-muted-foreground">{$t('admin.status')}</Table.Head>
							<Table.Head class="text-[10px] uppercase tracking-wider text-muted-foreground">{$t('admin.expires')}</Table.Head>
							<Table.Head class="w-[1%]"></Table.Head>
						</Table.Row>
					</Table.Header>
					<Table.Body>
						{#each invitations as inv (inv.id)}
							<Table.Row class="border-dashed border-border/80 transition-colors hover:bg-muted/40">
								<Table.Cell class="text-[12.5px] font-medium">{inv.email}</Table.Cell>
								<Table.Cell class="text-[12.5px] capitalize text-muted-foreground">{$t(`admin.roleName.${inv.role}`)}</Table.Cell>
								<Table.Cell>
									<Badge variant={STATUS_VARIANT[inv.status] ?? 'outline'} class="capitalize">
										{$t(`admin.inviteStatus.${inv.status}`)}
									</Badge>
								</Table.Cell>
								<Table.Cell class="text-[12px] tabular-nums text-muted-foreground">{fmtDate(inv.expires_at)}</Table.Cell>
								<Table.Cell class="w-[1%] whitespace-nowrap">
									{#if inv.status === 'pending'}
										<Tooltip.Root>
											<Tooltip.Trigger>
												<Button
													variant="ghost"
													size="icon"
													class="h-7 w-7 text-destructive hover:text-destructive"
													onclick={() => revoke(inv)}
												>
													<Icon name="trash-bin-trash" class="h-3.5 w-3.5" />
												</Button>
											</Tooltip.Trigger>
											<Tooltip.Content>{$t('admin.revoke')}</Tooltip.Content>
										</Tooltip.Root>
									{/if}
								</Table.Cell>
							</Table.Row>
						{:else}
							<Table.Row class="hover:bg-transparent">
								<Table.Cell colspan={5} class="p-0">
									<EmptyState icon="letter-unread" title={$t('admin.noInvites')} />
								</Table.Cell>
							</Table.Row>
						{/each}
					</Table.Body>
				</Table.Root>
			{/if}
		</Card.Root>
	</div>
{/if}

<!-- ===== Invite dialog ================================================= -->
<Dialog.Root bind:open={inviteOpen}>
	<Dialog.Content class="sm:max-w-md">
		<Dialog.Header>
			<Dialog.Title class="flex items-center gap-1.5 text-[14px]">
				<Icon name="user-plus-rounded" class="h-4 w-4 opacity-80" />
				{$t('admin.inviteTitle')}
			</Dialog.Title>
			<Dialog.Description class="text-[12px]">{$t('admin.inviteSubtitle')}</Dialog.Description>
		</Dialog.Header>

		{#if createdLink}
			<div class="space-y-3 py-1">
				<div class="text-pos flex items-center gap-1.5 text-[12.5px] font-medium">
					<Icon name="shield-check" class="h-4 w-4" />
					{$t('admin.inviteCreated')}
				</div>
				<p class="text-[11.5px] text-muted-foreground">{$t('admin.inviteShareHint')}</p>
				<div class="flex items-center gap-2">
					<Input value={createdLink} readonly class="font-mono text-[11px]" />
					<Button variant="outline" size="icon" class="h-9 w-9 shrink-0" onclick={copyLink}>
						{#if linkCopied}
							<Icon name="check-circle" active class="h-3.5 w-3.5 text-pos" />
						{:else}
							<Icon name="copy" class="h-3.5 w-3.5" />
						{/if}
					</Button>
				</div>
			</div>
			<Dialog.Footer>
				<Button onclick={() => (inviteOpen = false)}>{$t('admin.done')}</Button>
			</Dialog.Footer>
		{:else}
			<div class="space-y-4 py-1">
				<div class="space-y-1.5">
					<Label for="invite-email" class="text-[11.5px] text-muted-foreground">{$t('auth.email')}</Label>
					<Input id="invite-email" type="email" bind:value={inviteEmail} placeholder="person@example.com" />
				</div>
				<div class="space-y-1.5">
					<Label class="text-[11.5px] text-muted-foreground">{$t('admin.role')}</Label>
					<Select.Root type="single" bind:value={inviteRole}>
						<Select.Trigger class="text-[12.5px] capitalize">{$t(`admin.roleName.${inviteRole}`)}</Select.Trigger>
						<Select.Content>
							{#each ROLES as r}
								<Select.Item value={r} class="capitalize">{$t(`admin.roleName.${r}`)}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
				</div>
				{#if inviteError}
					<p class="text-destructive text-[12.5px]">{inviteError}</p>
				{/if}
			</div>
			<Dialog.Footer class="flex flex-row items-center justify-between gap-3 sm:justify-between">
				<Button variant="ghost" size="sm" onclick={() => (inviteOpen = false)}>{$t('common.cancel')}</Button>
				<Button size="sm" disabled={!inviteValid || inviting} onclick={submitInvite}>
					{#if inviting}
						<Spinner class="mr-1 h-3.5 w-3.5" />
					{:else}
						<Icon name="letter" class="mr-1 h-3.5 w-3.5" />
					{/if}
					{inviting ? $t('admin.sending') : $t('admin.sendInvite')}
				</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>
