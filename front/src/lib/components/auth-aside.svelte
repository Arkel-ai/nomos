<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import NomosMark from '$lib/components/nomos-mark.svelte';
	import AgentOrb from '$lib/components/thread/AgentOrb.svelte';
	import type { SolarIconName } from '$lib/icons/solar';
	import { t } from '$lib/i18n';

	/**
	 * The brand half of the auth pages (/login, /accept-invite): the product's
	 * line in the editorial serif, what Nomos is, and the orb at work. Hidden
	 * below lg, where the form stands alone.
	 */
	const pillars: { icon: SolarIconName; key: string }[] = [
		{ icon: 'scale', key: 'landing.forWhoTitle' },
		{ icon: 'routing-2', key: 'landing.missionTitle' },
		{ icon: 'book-bookmark', key: 'landing.sourcesTitle' },
		{ icon: 'shield-check', key: 'landing.controlTitle' }
	];
</script>

<aside
	class="bg-sidebar relative hidden overflow-hidden lg:flex lg:w-[52%] lg:flex-col lg:justify-between lg:p-12 xl:p-16"
>
	<div
		class="pointer-events-none absolute -bottom-40 -left-32 h-[520px] w-[520px] rounded-full blur-3xl"
		style="background: radial-gradient(closest-side, color-mix(in oklab, var(--brand) 14%, transparent), transparent)"
		aria-hidden="true"
	></div>

	<div class="rise-in relative flex items-center gap-2.5">
		<NomosMark size={28} />
		<span class="font-serif text-[22px] tracking-tight">{$t('title')}</span>
	</div>

	<div class="relative max-w-lg space-y-6">
		<h2 class="display text-foreground text-[52px] xl:text-[60px]">
			<span class="word-in inline-block" style="animation-delay: 120ms"
				>{$t('landing.headline')}</span
			>
			<span class="word-in inline-block" style="animation-delay: 260ms"
				><em>{$t('landing.headlineHighlight')}</em></span
			>
		</h2>
		<p class="rise-in text-muted-foreground max-w-md text-[15px] leading-relaxed" style="animation-delay: 360ms">
			{$t('landing.subtitle')}
		</p>
		<ul class="grid grid-cols-2 gap-x-6 gap-y-3 pt-2">
			{#each pillars as pillar, i}
				<li
					class="rise-in text-foreground/85 flex items-center gap-2.5 text-ui"
					style="animation-delay: {440 + i * 60}ms"
				>
					<span
						class="bg-card text-brand flex h-7 w-7 shrink-0 items-center justify-center rounded-lg shadow-xs"
					>
						<Icon name={pillar.icon} class="h-4 w-4" />
					</span>
					{$t(pillar.key)}
				</li>
			{/each}
		</ul>
	</div>

	<div class="relative flex items-center justify-between gap-4">
		<div class="text-muted-foreground flex items-center gap-2.5 text-xs">
			<AgentOrb state="composing" size={20} />
			{$t('tagline')}
		</div>
		<a
			href="https://arkel.ai"
			target="_blank"
			rel="noopener noreferrer"
			class="text-muted-foreground/70 hover:text-muted-foreground shrink-0 text-[10.5px] tracking-wide transition-colors"
		>
			{$t('common.poweredBy')}
		</a>
	</div>
</aside>
