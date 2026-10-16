<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/stores';
	import { authClient } from '$lib/auth/client';
	import { loadMe } from '$lib/stores/me';
	import { t } from '$lib/i18n';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Checkbox } from '$lib/components/ui/checkbox';
	import AuthAside from '$lib/components/auth-aside.svelte';
	import NomosMark from '$lib/components/nomos-mark.svelte';

	// Sign-in only: account creation is invite-only and happens on the
	// /accept-invite page (see docs/invitations.md). There is no
	// public sign-up form.
	let email = $state('');
	let password = $state('');
	let loading = $state(false);
	let error = $state('');

	// Second factor. Sign-in for a user with 2FA resolves with
	// `twoFactorRedirect` and no session; the session is issued once a TOTP
	// (or single-use backup) code verifies.
	let step = $state<'credentials' | 'code'>('credentials');
	let code = $state('');
	let useBackupCode = $state(false);
	let trustDevice = $state(false);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (!email || !password) {
			error = $t('auth.fillAllFields');
			return;
		}
		loading = true;
		error = '';
		try {
			const res = await authClient.signIn.email({ email, password });
			if (res.error) {
				error = res.error.message ?? $t('login.genericError');
				return;
			}
			if ('twoFactorRedirect' in res.data && res.data.twoFactorRedirect) {
				step = 'code';
				return;
			}
			await enterApp();
		} catch {
			error = $t('login.genericError');
		} finally {
			loading = false;
		}
	}

	async function verify(event: SubmitEvent) {
		event.preventDefault();
		const value = code.replace(/\s/g, '');
		if (!value) {
			error = $t('twoFactor.codeRequired');
			return;
		}
		loading = true;
		error = '';
		try {
			const res = useBackupCode
				? await authClient.twoFactor.verifyBackupCode({ code: value, trustDevice })
				: await authClient.twoFactor.verifyTotp({ code: value, trustDevice });
			if (res.error) {
				error = $t('twoFactor.invalidCode');
				return;
			}
			await enterApp();
		} catch {
			error = $t('login.genericError');
		} finally {
			loading = false;
		}
	}

	function toggleBackupCode() {
		useBackupCode = !useBackupCode;
		code = '';
		error = '';
	}

	function backToCredentials() {
		step = 'credentials';
		code = '';
		password = '';
		error = '';
	}

	async function enterApp() {
		await loadMe(true);
		// Back to the page the auth guard (+layout.ts) bounced us from —
		// same-origin only, so the param can't send the user off-site.
		const next = new URL($page.url.searchParams.get('redirect') ?? '/', location.origin);
		await goto(next.origin === location.origin ? next.pathname + next.search : '/');
	}
</script>

<svelte:head>
	<title>{$t('login.signInTitle')} · {$t('title')}</title>
</svelte:head>

<div class="bg-card flex min-h-svh">
	<AuthAside />

	<main class="relative flex flex-1 items-center justify-center px-6 py-12">
		<div class="w-full max-w-[360px] space-y-8">
			<div class="rise-in space-y-3">
				<NomosMark size={32} class="lg:hidden" />
				<h1 class="display text-foreground text-[34px]">
					{step === 'code' ? $t('twoFactor.title') : $t('login.signInTitle')}
				</h1>
				<p class="text-muted-foreground text-sm">
					{#if step === 'credentials'}
						{$t('auth.enterCredentials')}
					{:else if useBackupCode}
						{$t('twoFactor.enterBackupCode')}
					{:else}
						{$t('twoFactor.enterCode')}
					{/if}
				</p>
			</div>

			{#if step === 'code'}
				<form class="rise-in flex flex-col gap-5" style="animation-delay: 80ms" onsubmit={verify}>
					<div class="flex flex-col gap-2">
						<Label for="code" class="text-ui">
							{useBackupCode ? $t('twoFactor.backupCode') : $t('twoFactor.code')}
						</Label>
						<Input
							id="code"
							bind:value={code}
							autocomplete="one-time-code"
							inputmode={useBackupCode ? 'text' : 'numeric'}
							maxlength={useBackupCode ? 32 : 6}
							class="h-11 text-center font-mono text-lg tracking-[0.4em]"
						/>
					</div>

					<div class="flex items-center gap-2">
						<Checkbox id="trust-device" bind:checked={trustDevice} />
						<Label for="trust-device" class="text-muted-foreground text-ui font-normal">
							{$t('twoFactor.trustDevice')}
						</Label>
					</div>

					{#if error}
						<p class="text-destructive text-ui">{error}</p>
					{/if}

					<Button type="submit" disabled={loading} class="h-10 w-full">
						{#if loading}
							<Spinner class="h-4 w-4" />
							{$t('twoFactor.verifying')}
						{:else}
							{$t('twoFactor.verify')}
						{/if}
					</Button>

					<div class="flex items-center justify-between text-ui">
						<button
							type="button"
							onclick={backToCredentials}
							class="text-muted-foreground hover:text-foreground inline-flex cursor-pointer items-center gap-1.5 transition-colors"
						>
							<Icon name="arrow-left" class="h-3.5 w-3.5" />
							{$t('common.back')}
						</button>
						<button type="button" onclick={toggleBackupCode} class="motion-link text-brand cursor-pointer">
							{useBackupCode ? $t('twoFactor.useApp') : $t('twoFactor.useBackupCode')}
						</button>
					</div>
				</form>
			{:else}
				<form class="rise-in flex flex-col gap-5" style="animation-delay: 80ms" onsubmit={submit}>
					<div class="flex flex-col gap-2">
						<Label for="email" class="text-ui">{$t('auth.email')}</Label>
						<Input id="email" type="email" bind:value={email} autocomplete="email" class="h-10" />
					</div>
					<div class="flex flex-col gap-2">
						<Label for="password" class="text-ui">{$t('auth.password')}</Label>
						<Input
							id="password"
							type="password"
							bind:value={password}
							autocomplete="current-password"
							class="h-10"
						/>
					</div>

					{#if error}
						<p class="text-destructive text-ui">{error}</p>
					{/if}

					<Button type="submit" disabled={loading} class="h-10 w-full">
						{#if loading}
							<Spinner class="h-4 w-4" />
							{$t('auth.signingIn')}
						{:else}
							{$t('auth.signIn')}
							<Icon name="arrow-right" class="h-4 w-4" />
						{/if}
					</Button>
				</form>

				<p
					class="rise-in border-border text-muted-foreground flex items-start gap-2 border-t pt-5 text-xs leading-relaxed"
					style="animation-delay: 140ms"
				>
					<Icon name="lock" class="mt-px h-3.5 w-3.5 shrink-0" />
					{$t('login.inviteOnly')}
				</p>
			{/if}
		</div>
	</main>
</div>
