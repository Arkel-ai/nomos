<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import Spinner from '$lib/components/spinner.svelte';
	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { authClient } from '$lib/auth/client';
	import { validateInvitation, acceptInvitation } from '$lib/api/invitations';
	import { loadMe } from '$lib/stores/me';
	import { t } from '$lib/i18n';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Skeleton } from '$lib/components/ui/skeleton';
	import AuthAside from '$lib/components/auth-aside.svelte';
	import NomosMark from '$lib/components/nomos-mark.svelte';

	type Phase = 'validating' | 'invalid' | 'form' | 'submitting';

	let phase = $state<Phase>('validating');
	let reason = $state('');
	let email = $state('');
	let role = $state('');
	let name = $state('');
	let password = $state('');
	let error = $state('');

	const token = $derived($page.url.searchParams.get('token') ?? '');

	onMount(async () => {
		if (!token) {
			reason = 'missing';
			phase = 'invalid';
			return;
		}
		const { data } = await validateInvitation(token);
		if (data?.valid) {
			email = data.email ?? '';
			role = data.role ?? '';
			phase = 'form';
		} else {
			reason = data?.reason ?? 'invalid';
			phase = 'invalid';
		}
	});

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		if (!name || !password) {
			error = $t('auth.fillAllFields');
			return;
		}
		phase = 'submitting';
		error = '';
		try {
			// Create the Better Auth account (gated to invited emails) — this
			// also signs the user in (autoSignIn). Then redeem the token so the
			// API grants the invited role to this now-authenticated user.
			const res = await authClient.signUp.email({ name, email, password });
			if (res.error) {
				error = res.error.message ?? $t('accept.genericError');
				phase = 'form';
				return;
			}
			const accept = await acceptInvitation(token);
			if (accept.status !== 200) {
				error = accept.data?.detail ?? $t('accept.genericError');
				phase = 'form';
				return;
			}
			await loadMe(true);
			await goto('/');
		} catch {
			error = $t('accept.genericError');
			phase = 'form';
		}
	}
</script>

<svelte:head>
	<title>{$t('accept.title')} · {$t('title')}</title>
</svelte:head>

<div class="bg-card flex min-h-svh">
	<AuthAside />

	<main class="relative flex flex-1 items-center justify-center px-6 py-12">
		<div class="w-full max-w-[360px] space-y-8">
			{#if phase === 'validating'}
				<div class="space-y-4" aria-busy="true">
					<Skeleton class="h-10 w-10 rounded-xl" />
					<Skeleton class="h-8 w-56" />
					<Skeleton class="h-4 w-72 max-w-full" />
					<Skeleton class="mt-6 h-10 w-full rounded-xl" />
					<Skeleton class="h-10 w-full rounded-xl" />
				</div>
			{:else if phase === 'invalid'}
				<div class="rise-in space-y-4">
					<span
						class="bg-destructive/10 text-destructive flex h-10 w-10 items-center justify-center rounded-xl"
					>
						<Icon name="shield-cross" active class="h-5 w-5" />
					</span>
					<h1 class="display text-foreground text-[34px]">{$t('accept.invalidTitle')}</h1>
					<p class="text-muted-foreground text-sm">{$t(`accept.reason.${reason}`)}</p>
				</div>
				<Button variant="outline" href="/login" class="rise-in h-10 w-full" style="animation-delay: 80ms">
					{$t('auth.signIn')}
				</Button>
			{:else}
				<div class="rise-in space-y-3">
					<NomosMark size={32} class="lg:hidden" />
					<span
						class="bg-brand-soft text-brand hidden h-10 w-10 items-center justify-center rounded-xl lg:flex"
					>
						<Icon name="letter-opened" active class="h-5 w-5" />
					</span>
					<h1 class="display text-foreground text-[34px]">{$t('accept.title')}</h1>
					<p class="text-muted-foreground text-sm">{$t('accept.subtitle')}</p>
				</div>
				<form class="rise-in flex flex-col gap-5" style="animation-delay: 80ms" onsubmit={submit}>
					<div class="flex flex-col gap-2">
						<Label for="email" class="text-ui">{$t('auth.email')}</Label>
						<Input id="email" type="email" value={email} disabled class="h-10 font-mono text-ui" />
					</div>
					<div class="flex flex-col gap-2">
						<Label for="name" class="text-ui">{$t('login.name')}</Label>
						<Input id="name" bind:value={name} autocomplete="name" class="h-10" />
					</div>
					<div class="flex flex-col gap-2">
						<Label for="password" class="text-ui">{$t('accept.choosePassword')}</Label>
						<Input
							id="password"
							type="password"
							bind:value={password}
							autocomplete="new-password"
							class="h-10"
						/>
					</div>

					{#if error}
						<p class="text-destructive text-ui">{error}</p>
					{/if}

					<Button type="submit" disabled={phase === 'submitting'} class="h-10 w-full">
						{#if phase === 'submitting'}
							<Spinner class="h-4 w-4" />
							{$t('accept.creating')}
						{:else}
							{$t('accept.cta')}
							<Icon name="arrow-right" class="h-4 w-4" />
						{/if}
					</Button>
				</form>
			{/if}
		</div>
	</main>
</div>
