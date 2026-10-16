<script lang="ts">
	// Svelte port of border-beam's BorderBeam (libraries.dev/beam, MIT — see
	// ../LICENSE). styles.ts and pulseDriver.ts are upstream verbatim; only
	// this wrapper differs: `theme="auto"` follows mode-watcher instead of
	// prefers-color-scheme, so the in-app theme toggle drives it.
	import { untrack } from 'svelte';
	import { mode } from 'mode-watcher';
	import type { BorderBeamProps } from './types';
	import { sizePresets, sizeThemePresets, generateBeamCSS, getPulseDriverConfig } from './styles';
	import { registerPulseInstance } from './pulseDriver';

	let {
		children,
		size = 'md',
		colorVariant = 'colorful',
		theme = 'auto',
		staticColors = false,
		duration,
		active = true,
		borderRadius: customBorderRadius,
		brightness: brightnessProp,
		saturation,
		hueRange = 30,
		glowSize = 1,
		strength = 1,
		css: extraCss,
		onActivate,
		onDeactivate,
		onanimationend,
		ref = $bindable(null),
		...rest
	}: BorderBeamProps & { ref?: HTMLDivElement | null } = $props();

	const id = $props.id();

	let isActive = $state(untrack(() => active));
	let isFading = $state(false);
	let isVisible = $state(true);
	let detectedRadius = $state<number | null>(null);
	let pulseGlowScale = $state({ x: 1, y: 1 });

	// Auto-detect child border radius when no explicit value is provided
	$effect(() => {
		const el = ref;
		if (customBorderRadius != null || !el) return;

		const detect = () => {
			const child = el.firstElementChild as HTMLElement | null;
			if (!child) return;
			const raw = parseFloat(getComputedStyle(child).borderTopLeftRadius);
			if (!isNaN(raw) && raw > 0) detectedRadius = raw;
		};

		detect();
		// Re-detect if child layout changes (e.g. CSS loaded late)
		const observer = new MutationObserver(detect);
		observer.observe(el, { childList: true, subtree: false });
		return () => observer.disconnect();
	});

	$effect(() => {
		if (active && !isActive && !isFading) isActive = true;
		else if (!active && isActive && !isFading) isFading = true;
	});

	// Pause the (paint-heavy) animations while the element is scrolled offscreen,
	// without changing the logical active/fading state.
	$effect(() => {
		const el = ref;
		if (!el || typeof IntersectionObserver === 'undefined') return;
		const observer = new IntersectionObserver(
			(entries) => {
				for (const entry of entries) isVisible = entry.isIntersecting;
			},
			// Start animating slightly before the element scrolls into view.
			{ rootMargin: '256px' }
		);
		observer.observe(el);
		return () => observer.disconnect();
	});

	// Pulse Outside glow geometry is authored for a ~350x140 element; scale it
	// per-axis so the halo fits whatever it wraps.
	$effect(() => {
		const el = ref;
		if (size !== 'pulse-outside') {
			pulseGlowScale = { x: 1, y: 1 };
			return;
		}
		if (!el) return;

		const REF_WIDTH = 350;
		const REF_HEIGHT = 140;
		const clamp = (value: number) => Math.max(0.35, Math.min(4, value));

		const measure = () => {
			const child = el.firstElementChild as HTMLElement | null;
			if (!child) return;
			const rect = child.getBoundingClientRect();
			if (!rect.width || !rect.height) return;
			const x = +clamp(rect.width / REF_WIDTH).toFixed(3);
			const y = +clamp(rect.height / REF_HEIGHT).toFixed(3);
			const prev = untrack(() => pulseGlowScale);
			if (prev.x !== x || prev.y !== y) pulseGlowScale = { x, y };
		};

		measure();
		const child = el.firstElementChild as HTMLElement | null;
		if (typeof ResizeObserver === 'undefined' || !child) return;
		const resizeObserver = new ResizeObserver(measure);
		resizeObserver.observe(child);
		return () => resizeObserver.disconnect();
	});

	function handleAnimationEnd(e: AnimationEvent & { currentTarget: EventTarget & HTMLDivElement }) {
		if (e.animationName.includes('fade-out')) {
			isActive = false;
			isFading = false;
			onDeactivate?.();
		} else if (e.animationName.includes('fade-in')) {
			onActivate?.();
		}
		onanimationend?.(e);
	}

	const resolvedTheme = $derived(theme === 'auto' ? (mode.current ?? 'dark') : theme);
	const themeConfig = $derived(sizeThemePresets[size][resolvedTheme]);
	const sizeConfig = $derived(sizePresets[size]);
	const isPulse = $derived(size === 'pulse-inner' || size === 'pulse-outside');

	const finalBorderRadius = $derived(customBorderRadius ?? detectedRadius ?? sizeConfig.borderRadius);
	const finalDuration = $derived(duration ?? (size === 'line' ? 3.1 : isPulse ? 2.3 : 1.96));
	const finalSaturation = $derived(saturation ?? themeConfig.saturation);
	const finalBrightness = $derived(brightnessProp ?? themeConfig.brightness ?? 1.3);
	const finalHueRange = $derived(size === 'line' ? Math.min(hueRange, 13) : hueRange);
	const finalStaticColors = $derived(colorVariant === 'mono' ? true : staticColors);

	const cssStyles = $derived(
		generateBeamCSS({
			id,
			borderRadius: finalBorderRadius,
			borderWidth: sizeConfig.borderWidth,
			duration: finalDuration,
			strokeOpacity: themeConfig.strokeOpacity,
			innerOpacity: themeConfig.innerOpacity,
			bloomOpacity: themeConfig.bloomOpacity,
			innerShadow: themeConfig.innerShadow,
			size,
			colorVariant,
			staticColors: finalStaticColors,
			brightness: finalBrightness,
			saturation: finalSaturation,
			hueRange: finalHueRange,
			theme: resolvedTheme,
			hairlineOpacity: themeConfig.hairlineOpacity,
			glowSize
		})
	);
	const styleSheet = $derived(extraCss ? `${cssStyles}\n${extraCss.split('{id}').join(id)}` : cssStyles);

	// Runtime config for the JS breathing driver (null for non-pulse sizes).
	const driverConfig = $derived(
		isPulse
			? getPulseDriverConfig(size, resolvedTheme, finalDuration, finalHueRange, finalStaticColors, id)
			: null
	);

	// Drive the Pulse breathing from the shared, fps-capped rAF loop while the
	// instance is on, onscreen, and the user hasn't requested reduced motion.
	$effect(() => {
		const el = ref;
		const config = driverConfig;
		if (!config || !el) return;
		if (!(isActive || isFading) || !isVisible) return;
		if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return;
		return registerPulseInstance(el, config);
	});
</script>

{@html `<style>${styleSheet}</style>`}
<div
	{...rest}
	bind:this={ref}
	data-beam={id}
	data-active={isActive && !isFading ? '' : undefined}
	data-fading={isFading ? '' : undefined}
	data-paused={isActive && !isFading && !isVisible ? '' : undefined}
	style:--beam-strength={Math.max(0, Math.min(1, strength))}
	style:--pulse-glow-sx={size === 'pulse-outside' ? pulseGlowScale.x : undefined}
	style:--pulse-glow-sy={size === 'pulse-outside' ? pulseGlowScale.y : undefined}
	onanimationend={handleAnimationEnd}
>
	{@render children()}
	<div data-beam-bloom></div>
</div>
