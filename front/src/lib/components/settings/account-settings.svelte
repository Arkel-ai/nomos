<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	// The account tab of /settings. The
	// account block reads the live Better Auth session, the password form calls
	// `/auth/change-password` and the two-factor block drives the Better Auth
	// `twoFactor` plugin. Every request runs as the signed-in user; the plugin
	// re-checks the password before turning 2FA on or off.
	import { renderSVG } from 'uqr';
	import { authClient } from '$lib/auth/client';
	import { t, locale } from '$lib/i18n';
	import * as Card from '$lib/components/ui/card';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { Switch } from '$lib/components/ui/switch';
	import { Separator } from '$lib/components/ui/separator';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import { sileo } from '$lib/components/ui/sileo';

	// Better Auth's default `minPasswordLength`; keep in sync if the server
	// config in `$lib/auth/server.ts` ever overrides it.
	const MIN_PASSWORD_LENGTH = 8;

	const session = authClient.useSession();
	const account = $derived($session.data?.user);
	const accountName = $derived(account?.name || 'User');
	const accountInitial = $derived(accountName.trim().charAt(0).toUpperCase() || 'U');
	const memberSince = $derived(
		account?.createdAt ? new Date(account.createdAt).toLocaleDateString($locale) : ''
	);

	// ── Password ──
	let currentPassword = $state('');
	let newPassword = $state('');
	let confirmPassword = $state('');
	let revokeOtherSessions = $state(true);
	let submitting = $state(false);
	let error = $state('');

	const valid = $derived(
		currentPassword.length > 0 &&
			newPassword.length >= MIN_PASSWORD_LENGTH &&
			confirmPassword === newPassword
	);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (submitting) return;
		if (newPassword.length < MIN_PASSWORD_LENGTH) {
			error = $t('settings.passwordTooShort');
			return;
		}
		if (newPassword !== confirmPassword) {
			error = $t('settings.passwordMismatch');
			return;
		}
		submitting = true;
		error = '';
		try {
			const res = await authClient.changePassword({
				currentPassword,
				newPassword,
				revokeOtherSessions
			});
			if (res.error) {
				error = res.error.message ?? $t('settings.passwordError');
				return;
			}
			currentPassword = '';
			newPassword = '';
			confirmPassword = '';
			sileo.success({ title: $t('settings.passwordChanged') });
		} catch {
			error = $t('settings.passwordError');
		} finally {
			submitting = false;
		}
	}

	// ── Two-factor ──
	// `enable` returns the TOTP URI and backup codes, but 2FA only switches on
	// once a code from the authenticator verifies — abandoning setup halfway
	// leaves sign-in unchanged. Backup codes are shown once, after that.
	let tfStep = $state<'idle' | 'password' | 'scan' | 'backup'>('idle');
	let tfPassword = $state('');
	let tfCode = $state('');
	let tfBusy = $state(false);
	let tfError = $state('');
	let totpURI = $state('');
	let backupCodes = $state<string[]>([]);

	const twoFactorEnabled = $derived(!!account?.twoFactorEnabled);
	// Black modules on white in both themes: scanners need the contrast.
	const qrSvg = $derived(totpURI ? renderSVG(totpURI, { border: 2 }) : '');
	const totpSecret = $derived(totpURI ? (new URL(totpURI).searchParams.get('secret') ?? '') : '');

	function resetTwoFactor() {
		tfStep = 'idle';
		tfPassword = '';
		tfCode = '';
		tfError = '';
		totpURI = '';
		backupCodes = [];
	}

	async function submitTwoFactorPassword(event: SubmitEvent) {
		event.preventDefault();
		if (tfBusy || !tfPassword) return;
		tfBusy = true;
		tfError = '';
		try {
			const res = twoFactorEnabled
				? await authClient.twoFactor.disable({ password: tfPassword })
				: await authClient.twoFactor.enable({ password: tfPassword });
			if (res.error) {
				tfError =
					res.error.code === 'INVALID_PASSWORD'
						? $t('twoFactor.wrongPassword')
						: $t('twoFactor.error');
				return;
			}
			if (res.data && 'totpURI' in res.data) {
				totpURI = res.data.totpURI;
				backupCodes = res.data.backupCodes;
				tfPassword = '';
				tfStep = 'scan';
			} else {
				resetTwoFactor();
				sileo.success({ title: $t('twoFactor.disabled') });
			}
		} catch {
			tfError = $t('twoFactor.error');
		} finally {
			tfBusy = false;
		}
	}

	async function verifySetup(event: SubmitEvent) {
		event.preventDefault();
		const code = tfCode.replace(/\s/g, '');
		if (code.length !== 6) {
			tfError = $t('twoFactor.codeRequired');
			return;
		}
		tfBusy = true;
		tfError = '';
		try {
			const res = await authClient.twoFactor.verifyTotp({ code });
			if (res.error) {
				tfError = $t('twoFactor.invalidCode');
				return;
			}
			tfCode = '';
			tfStep = 'backup';
			sileo.success({ title: $t('twoFactor.enabled') });
		} catch {
			tfError = $t('twoFactor.error');
		} finally {
			tfBusy = false;
		}
	}

	async function copy(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			sileo.success({ title: $t('common.copied') });
		} catch {
			sileo.error({ title: $t('common.error') });
		}
	}

	function downloadBackupCodes() {
		const url = URL.createObjectURL(new Blob([backupCodes.join('\n') + '\n'], { type: 'text/plain' }));
		const a = document.createElement('a');
		a.href = url;
		a.download = 'backup-codes.txt';
		a.click();
		URL.revokeObjectURL(url);
	}
</script>

<div class="space-y-4">
	<!-- ===== Account card ============================================== -->
	<Card.Root class="gap-4 rounded-2xl p-5">
		<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
			<Icon name="user-rounded" class="text-muted-foreground h-4 w-4" />
			{$t('settings.account')}
		</div>

		{#if !account}
			<div class="flex items-center gap-3">
				<Skeleton class="h-11 w-11 rounded-full" />
				<div class="flex-1 space-y-2">
					<Skeleton class="h-3.5 w-40" />
					<Skeleton class="h-3 w-56" />
				</div>
			</div>
		{:else}
			<div class="flex items-center gap-3">
				<div
					class="flex h-11 w-11 shrink-0 items-center justify-center rounded-full bg-gradient-to-br from-primary to-primary/70 text-[16px] font-semibold text-primary-foreground shadow-[inset_0_-2px_4px_rgba(0,0,0,0.08)]"
				>
					{accountInitial}
				</div>
				<div class="min-w-0 flex-1 space-y-0.5">
					<div class="flex items-center gap-2">
						<span class="truncate text-[13.5px] font-semibold">{accountName}</span>
						<Badge variant={account.emailVerified ? 'secondary' : 'outline'} class="h-4 px-1.5 text-[9.5px]">
							{account.emailVerified ? $t('settings.emailVerified') : $t('settings.emailUnverified')}
						</Badge>
					</div>
					<div class="truncate text-[11.5px] text-muted-foreground">{account.email}</div>
				</div>
			</div>

			{#if memberSince}
				<Separator class="border-dashed" />
				<div class="space-y-0.5">
					<div class="text-[10.5px] font-medium uppercase tracking-wider text-muted-foreground/70">
						{$t('settings.memberSince')}
					</div>
					<div class="text-[12.5px] font-medium">{memberSince}</div>
				</div>
			{/if}
		{/if}
	</Card.Root>

	<!-- ===== Password card ============================================= -->
	<Card.Root class="gap-4 rounded-2xl p-5">
		<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
			<Icon name="shield" class="text-muted-foreground h-4 w-4" />
			{$t('settings.security')}
		</div>

		<form class="contents" onsubmit={submit}>
			<!-- Hidden username field: password managers need it to associate the
			     new credential with this account. -->
			<input
				type="text"
				name="username"
				autocomplete="username"
				value={account?.email ?? ''}
				readonly
				hidden
			/>

			<div class="space-y-3">
				<div class="flex items-start gap-3">
					<div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-muted">
						<Icon name="key" class="h-3.5 w-3.5 text-muted-foreground" />
					</div>
					<div class="space-y-0.5">
						<div class="text-[12.5px] font-medium">{$t('settings.changePassword')}</div>
						<div class="text-[11.5px] text-muted-foreground">
							{$t('settings.changePasswordHint')}
						</div>
					</div>
				</div>

				<div class="space-y-1.5">
					<Label for="current-password" class="text-[11.5px] text-muted-foreground">
						{$t('settings.currentPassword')}
					</Label>
					<Input
						id="current-password"
						type="password"
						bind:value={currentPassword}
						autocomplete="current-password"
					/>
				</div>

				<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
					<div class="space-y-1.5">
						<Label for="new-password" class="text-[11.5px] text-muted-foreground">
							{$t('settings.newPassword')}
						</Label>
						<Input
							id="new-password"
							type="password"
							bind:value={newPassword}
							autocomplete="new-password"
						/>
					</div>
					<div class="space-y-1.5">
						<Label for="confirm-password" class="text-[11.5px] text-muted-foreground">
							{$t('settings.confirmPassword')}
						</Label>
						<Input
							id="confirm-password"
							type="password"
							bind:value={confirmPassword}
							autocomplete="new-password"
						/>
					</div>
				</div>

				<div
					class="flex items-center justify-between gap-4 rounded-md border border-dashed border-border/80 p-3"
				>
					<div class="space-y-0.5">
						<div class="text-[12.5px] font-medium">{$t('settings.revokeOthers')}</div>
						<div class="text-[11.5px] text-muted-foreground">
							{$t('settings.revokeOthersHint')}
						</div>
					</div>
					<Switch bind:checked={revokeOtherSessions} />
				</div>
			</div>

			{#if error}
				<p class="text-[12.5px] text-destructive">{error}</p>
			{/if}

			<Separator class="border-dashed" />

			<div class="flex items-center justify-end">
				<Button type="submit" size="sm" disabled={!valid || submitting}>
					{#if submitting}
						<Spinner class="mr-1 h-3.5 w-3.5" />
					{:else}
						<Icon name="diskette" class="mr-1 h-3.5 w-3.5" />
					{/if}
					{submitting ? $t('settings.updating') : $t('settings.updatePassword')}
				</Button>
			</div>
		</form>
	</Card.Root>

	<!-- ===== Two-factor card =========================================== -->
	<Card.Root class="gap-4 rounded-2xl p-5">
		<div class="text-foreground flex items-center gap-2 text-ui font-semibold">
			<Icon name="shield-check" class="text-muted-foreground h-4 w-4" />
			{$t('twoFactor.title')}
		</div>

		<div class="flex flex-wrap items-start justify-between gap-3">
			<div class="flex items-start gap-3">
				<div class="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-muted">
					<Icon name="smartphone" class="h-3.5 w-3.5 text-muted-foreground" />
				</div>
				<div class="space-y-0.5">
					<div class="flex items-center gap-2 text-[12.5px] font-medium">
						{$t('twoFactor.authenticatorApp')}
						<Badge variant={twoFactorEnabled ? 'default' : 'outline'} class="h-4 px-1.5 text-[9.5px]">
							{twoFactorEnabled ? $t('twoFactor.on') : $t('twoFactor.off')}
						</Badge>
					</div>
					<div class="text-[11.5px] text-muted-foreground">{$t('twoFactor.hint')}</div>
				</div>
			</div>
			{#if tfStep === 'idle'}
				<Button
					variant={twoFactorEnabled ? 'outline' : 'default'}
					size="sm"
					class="shrink-0"
					disabled={!account}
					onclick={() => (tfStep = 'password')}
				>
					{twoFactorEnabled ? $t('twoFactor.disable') : $t('twoFactor.enable')}
				</Button>
			{/if}
		</div>

		{#if tfStep === 'password'}
			<form class="space-y-3" onsubmit={submitTwoFactorPassword}>
				<input
					type="text"
					name="username"
					autocomplete="username"
					value={account?.email ?? ''}
					readonly
					hidden
				/>
				<div class="space-y-1.5">
					<Label for="tf-password" class="text-[11.5px] text-muted-foreground">
						{$t('twoFactor.confirmPassword')}
					</Label>
					<Input
						id="tf-password"
						type="password"
						bind:value={tfPassword}
						autocomplete="current-password"
					/>
				</div>
				{#if tfError}
					<p class="text-[12.5px] text-destructive">{tfError}</p>
				{/if}
				<div class="flex items-center justify-end gap-2">
					<Button variant="ghost" size="sm" onclick={resetTwoFactor}>{$t('common.cancel')}</Button>
					<Button
						type="submit"
						size="sm"
						variant={twoFactorEnabled ? 'destructive' : 'default'}
						disabled={!tfPassword || tfBusy}
					>
						{#if tfBusy}
							<Spinner class="mr-1 h-3.5 w-3.5" />
						{/if}
						{twoFactorEnabled ? $t('twoFactor.disable') : $t('common.next')}
					</Button>
				</div>
			</form>
		{:else if tfStep === 'scan'}
			<Separator class="border-dashed" />
			<div class="grid gap-5 sm:grid-cols-[auto_minmax(0,1fr)]">
				<div
					class="self-start overflow-hidden rounded-lg border border-border/60 [&_svg]:h-40 [&_svg]:w-40"
				>
					{@html qrSvg}
				</div>
				<div class="min-w-0 space-y-4">
					<div class="space-y-1.5">
						<div class="text-[12.5px] font-medium">1. {$t('twoFactor.scanTitle')}</div>
						<div class="text-[11.5px] text-muted-foreground">{$t('twoFactor.scanHint')}</div>
						<div class="flex items-center gap-1">
							<code class="truncate rounded-md bg-muted px-2 py-1 font-mono text-[11.5px]">
								{totpSecret}
							</code>
							<Button
								variant="ghost"
								size="icon"
								class="h-7 w-7 shrink-0"
								aria-label={$t('common.copy')}
								onclick={() => copy(totpSecret)}
							>
								<Icon name="copy" class="h-3.5 w-3.5" />
							</Button>
						</div>
					</div>

					<form class="space-y-3" onsubmit={verifySetup}>
						<div class="space-y-1.5">
							<Label for="tf-code" class="text-[12.5px] font-medium">
								2. {$t('twoFactor.verifyTitle')}
							</Label>
							<Input
								id="tf-code"
								bind:value={tfCode}
								inputmode="numeric"
								autocomplete="one-time-code"
								maxlength={6}
								class="w-40 text-center font-mono tracking-[0.3em]"
							/>
						</div>
						{#if tfError}
							<p class="text-[12.5px] text-destructive">{tfError}</p>
						{/if}
						<div class="flex items-center justify-end gap-2">
							<Button variant="ghost" size="sm" onclick={resetTwoFactor}>
								{$t('common.cancel')}
							</Button>
							<Button type="submit" size="sm" disabled={tfBusy}>
								{#if tfBusy}
									<Spinner class="mr-1 h-3.5 w-3.5" />
								{/if}
								{$t('twoFactor.verifyAndEnable')}
							</Button>
						</div>
					</form>
				</div>
			</div>
		{:else if tfStep === 'backup'}
			<Separator class="border-dashed" />
			<div class="space-y-3">
				<div class="space-y-0.5">
					<div class="text-[12.5px] font-medium">{$t('twoFactor.backupTitle')}</div>
					<div class="text-[11.5px] text-muted-foreground">{$t('twoFactor.backupHint')}</div>
				</div>
				<div
					class="grid grid-cols-2 gap-x-4 gap-y-1.5 rounded-md border border-dashed border-border/80 p-3 font-mono text-[12.5px] tabular-nums sm:grid-cols-5"
				>
					{#each backupCodes as backupCode}
						<span>{backupCode}</span>
					{/each}
				</div>
				<div class="flex flex-wrap items-center justify-end gap-2">
					<Button variant="outline" size="sm" onclick={() => copy(backupCodes.join('\n'))}>
						<Icon name="copy" class="h-3.5 w-3.5" />
						{$t('common.copy')}
					</Button>
					<Button variant="outline" size="sm" onclick={downloadBackupCodes}>
						<Icon name="download" class="h-3.5 w-3.5" />
						{$t('common.download')}
					</Button>
					<Button size="sm" onclick={resetTwoFactor}>{$t('twoFactor.savedCodes')}</Button>
				</div>
			</div>
		{/if}
	</Card.Root>
</div>
