<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { Button } from '$lib/components/ui/button';
	import { reveal } from '$lib/motion';
	import NomosMark from '$lib/components/nomos-mark.svelte';
	import LandingDemo from '$lib/components/landing-demo.svelte';
	import type { SolarIconName } from '$lib/icons/solar';
	import { t } from '$lib/i18n';
	import { loadMe } from '$lib/stores/me';

	/**
	 * The signed-out landing. An authenticated visitor never sees it: the app is
	 * chat-first, so `/` redirects to `/chat` as soon as the session resolves
	 * (docs/PLAN.md §11).
	 */
	let checkingSession = $state(true);

	onMount(async () => {
		const me = await loadMe();
		if (me) {
			await goto('/chat', { replaceState: true });
			return;
		}
		checkingSession = false;
	});

	const REPO_URL = 'https://github.com/Arkel-ai/nomos';

	type Pillar = { icon: SolarIconName; titleKey: string; bodyKey: string };

	const pillars: Pillar[] = [
		{ icon: 'scale', titleKey: 'landing.forWhoTitle', bodyKey: 'landing.forWhoBody' },
		{ icon: 'checklist', titleKey: 'landing.missionTitle', bodyKey: 'landing.missionBody' },
		{ icon: 'book-bookmark', titleKey: 'landing.sourcesTitle', bodyKey: 'landing.sourcesBody' },
		{ icon: 'shield-check', titleKey: 'landing.controlTitle', bodyKey: 'landing.controlBody' }
	];
</script>

<svelte:head>
	<title>{$t('title')} · {$t('landing.headline')} {$t('landing.headlineHighlight')}</title>
</svelte:head>

<div class="bg-background min-h-svh transition-opacity duration-300" class:opacity-0={checkingSession}>
	<!-- ===== Nav ========================================================= -->
	<header class="bg-background/80 sticky top-0 z-30 backdrop-blur-xl">
		<div class="mx-auto flex h-16 max-w-7xl items-center justify-between px-6">
			<a href="/" class="flex items-center gap-2.5">
				<NomosMark size={26} />
				<span class="text-foreground font-serif text-[21px] tracking-tight">{$t('title')}</span>
			</a>
			<div class="flex items-center gap-1.5">
				<Button
					variant="ghost"
					href={REPO_URL}
					target="_blank"
					rel="noopener"
					class="text-muted-foreground hover:text-foreground hidden h-9 px-3 text-ui font-medium sm:inline-flex"
				>
					<Icon name="code" class="h-4 w-4" />
					{$t('landing.github')}
				</Button>
				<Button href="/login" class="h-9 px-4 text-ui font-medium">
					{$t('landing.ctaSecondary')}
				</Button>
			</div>
		</div>
	</header>

	<!-- ===== Hero ======================================================== -->
	<section class="mx-auto grid max-w-7xl items-center gap-14 px-6 pt-14 pb-24 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)] lg:pt-20">
		<div class="space-y-8">
			<span
				class="rise-in bg-card text-muted-foreground inline-flex h-8 items-center gap-2 rounded-full border px-3 text-xs font-medium shadow-xs"
			>
				<span
					class="from-brand to-signal h-1.5 w-1.5 rounded-full bg-gradient-to-br shadow-[0_0_8px_var(--signal)]"
					aria-hidden="true"
				></span>
				{$t('landing.badge')}
			</span>
			<h1 class="display text-foreground text-[52px] sm:text-[64px] xl:text-[72px]">
				{#each $t('landing.headline').split(' ') as word, i}
					<span class="word-in inline-block" style="animation-delay: {100 + i * 80}ms">{word}</span>{' '}
				{/each}
				<br />
				{#each $t('landing.headlineHighlight').split(' ') as word, i}
					<span class="word-in inline-block" style="animation-delay: {340 + i * 80}ms"
						><em>{word}</em></span
					>{' '}
				{/each}
			</h1>
			<p
				class="rise-in text-muted-foreground max-w-xl text-[17px] leading-relaxed"
				style="animation-delay: 520ms"
			>
				{$t('landing.subtitle')}
			</p>
			<div class="rise-in flex flex-wrap items-center gap-2.5" style="animation-delay: 620ms">
				<Button href="/login" class="group h-11 px-6 text-sm font-medium">
					{$t('landing.cta')}
					<Icon name="arrow-right" class="h-4 w-4 transition-transform group-hover:translate-x-0.5" />
				</Button>
				<Button
					variant="outline"
					href={REPO_URL}
					target="_blank"
					rel="noopener"
					class="bg-card h-11 px-5 text-sm font-medium"
				>
					<Icon name="code" class="h-4 w-4" />
					{$t('landing.github')}
				</Button>
			</div>
		</div>

		<div class="rise-in relative" style="animation-delay: 300ms">
			<div
				class="pointer-events-none absolute -inset-10 -z-0 rounded-full blur-3xl"
				style="background: radial-gradient(closest-side, color-mix(in oklab, var(--brand) 16%, transparent), transparent)"
				aria-hidden="true"
			></div>
			<div class="relative">
				<LandingDemo />
				<p class="text-muted-foreground mt-4 text-center text-xs">{$t('landing.demo.caption')}</p>
			</div>
		</div>
	</section>

	<!-- ===== What it is, and for whom ===================================== -->
	<section class="mx-auto max-w-7xl px-6 pb-20">
		<div class="grid gap-4 sm:grid-cols-2">
			{#each pillars as pillar, i}
				<article
					use:reveal={{ delay: i * 60 }}
					class="motion-card group bg-card relative overflow-hidden rounded-3xl border p-8"
				>
					<span
						class="bg-brand-soft text-brand flex h-11 w-11 items-center justify-center rounded-2xl"
					>
						<Icon name={pillar.icon} active class="h-5 w-5" />
					</span>
					<h2 class="text-foreground mt-7 font-serif text-[26px] leading-tight tracking-tight">
						{$t(pillar.titleKey)}
					</h2>
					<p class="text-muted-foreground mt-3 text-[15px] leading-relaxed">
						{$t(pillar.bodyKey)}
					</p>
				</article>
			{/each}
		</div>
	</section>

	<!-- ===== Open and auditable ========================================== -->
	<section class="mx-auto max-w-7xl px-6 pb-24">
		<div
			use:reveal
			class="bg-primary text-primary-foreground relative overflow-hidden rounded-[32px] px-8 py-12 md:px-14 md:py-16"
		>
			<div
				class="pointer-events-none absolute -top-24 -right-24 h-80 w-80 rounded-full blur-3xl"
				style="background: radial-gradient(closest-side, color-mix(in oklab, var(--brand) 45%, transparent), transparent)"
				aria-hidden="true"
			></div>
			<div class="relative flex flex-col gap-8 md:flex-row md:items-end md:justify-between">
				<div class="max-w-2xl space-y-4">
					<h2 class="font-serif text-[36px] leading-tight tracking-tight md:text-[44px]">
						{$t('landing.openSourceTitle')}
					</h2>
					<p class="text-primary-foreground/70 text-[15px] leading-relaxed">
						{$t('landing.openSourceBody')}
					</p>
				</div>
				<div class="flex shrink-0 flex-wrap items-center gap-2.5">
					<a
						href="/login"
						class="bg-primary-foreground text-primary inline-flex h-11 items-center gap-2 rounded-xl px-6 text-sm font-medium transition-transform hover:-translate-y-px"
					>
						{$t('landing.cta')}
						<Icon name="arrow-right" class="h-4 w-4" />
					</a>
					<a
						href={REPO_URL}
						target="_blank"
						rel="noopener"
						class="text-primary-foreground hover:bg-primary-foreground/10 inline-flex h-11 items-center gap-2 rounded-xl border border-current/20 px-5 text-sm font-medium transition-colors"
					>
						<Icon name="code" class="h-4 w-4" />
						{$t('landing.github')}
					</a>
				</div>
			</div>
		</div>
	</section>

	<!-- ===== Footer ====================================================== -->
	<footer class="border-border border-t">
		<div class="mx-auto flex max-w-7xl flex-col gap-6 px-6 py-10 md:flex-row md:items-center md:justify-between">
			<div class="flex items-center gap-2.5">
				<NomosMark size={20} />
				<span class="text-muted-foreground text-xs">{$t('tagline')}</span>
			</div>
			<p class="text-muted-foreground/80 max-w-xl text-xs leading-relaxed md:text-right">
				{$t('landing.disclaimer')}
			</p>
		</div>
	</footer>
</div>
