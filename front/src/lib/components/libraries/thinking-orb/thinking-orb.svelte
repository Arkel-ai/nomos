<script lang="ts">
	// Svelte port of thinking-orbs' ThinkingOrb (libraries.dev/orbs, MIT — see
	// ../LICENSE). engine/ and presets.ts are upstream verbatim. One shared
	// clock (performance.now) keeps every orb in phase; each instance pauses
	// while offscreen or when the tab is hidden, and reduced-motion users get a
	// static frame. `theme="auto"` follows mode-watcher. Upstream's optional
	// cursor `gravity` effect is not ported.
	import { mode } from 'mode-watcher';
	import { prefersReducedMotion } from 'svelte/motion';
	import { paintFrame, type OrbTint } from './engine/core';
	import { scaleCounts, scaleRadii } from './engine/profiles';
	import { MODE_FRAMES } from './engine/registry';
	import { resolvePreset } from './presets';
	import type { ThinkingOrbProps } from './types';

	/** Parse a CSS color into an RGB triple for the tinted ink painter.
	 *  Supports #rgb, #rrggbb and rgb()/rgba(); anything else -> no tint. */
	function parseTint(color: string | undefined): OrbTint | undefined {
		if (!color) return undefined;
		const hex = color.trim().match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/i);
		if (hex) {
			let h = hex[1];
			if (h.length === 3) h = h.replace(/./g, (c) => c + c);
			const n = parseInt(h, 16);
			return { r: (n >> 16) & 255, g: (n >> 8) & 255, b: n & 255 };
		}
		const fn = color.trim().match(/^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)/i);
		if (fn) return { r: Number(fn[1]), g: Number(fn[2]), b: Number(fn[3]) };
		return undefined;
	}

	const LABELS: Record<string, string> = {
		working: 'Working…',
		searching: 'Searching…',
		solving: 'Solving…',
		listening: 'Listening…',
		connecting: 'Connecting…',
		weaving: 'Weaving…',
		composing: 'Composing…',
		breathing: 'Thinking…',
		shaping: 'Shaping…'
	};

	let {
		state: orbState = 'working',
		size = 64,
		theme = 'auto',
		speed = 1,
		paused = false,
		color,
		dots = 1,
		dotSize = 1,
		opts: optsOverride,
		frame: customFrame,
		'aria-label': ariaLabel,
		...rest
	}: ThinkingOrbProps = $props();

	let canvas = $state<HTMLCanvasElement | null>(null);
	const dark = $derived(theme === 'auto' ? (mode.current ?? 'dark') === 'dark' : theme === 'dark');

	$effect(() => {
		if (!canvas) return;
		const dpr = Math.min(2, (typeof devicePixelRatio !== 'undefined' && devicePixelRatio) || 1);
		canvas.width = Math.round(size * dpr);
		canvas.height = Math.round(size * dpr);
		const ctx = canvas.getContext('2d');
		if (!ctx) return;

		const { mode: orbMode, speed: baseSpeed, opts: presetOpts } = resolvePreset(orbState, size);
		// `dots` / `dotSize` rescale the resolved preset with the same scalers the
		// presets use. resolvePreset caches — never mutate its result.
		let opts = dots !== 1 ? scaleCounts(presetOpts, Math.max(0.1, dots)) : presetOpts;
		if (dotSize !== 1) opts = scaleRadii(opts, Math.max(0.1, dotSize));
		// raw overrides land last, over everything the preset and multipliers set
		if (optsOverride) opts = { ...opts, ...optsOverride };
		const frameFn = customFrame ?? MODE_FRAMES[orbMode];
		const tint = parseTint(color);
		const effSpeed = baseSpeed * speed;
		const isDark = dark;
		const isPaused = paused;
		const px = size;

		const frame = (tSec: number) => {
			ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
			ctx.clearRect(0, 0, px, px);
			paintFrame(ctx, frameFn(px, tSec, opts), isDark, tint);
		};

		// reduced motion → one static, deterministic frame
		if (prefersReducedMotion.current) {
			frame(0.6);
			return;
		}

		let raf = 0;
		let running = false;
		const loop = () => {
			frame((performance.now() / 1000) * effSpeed);
			if (running) raf = requestAnimationFrame(loop);
		};
		const start = () => {
			if (running || isPaused) return;
			running = true;
			raf = requestAnimationFrame(loop);
		};
		const stop = () => {
			running = false;
			cancelAnimationFrame(raf);
		};

		// draw at least one frame even when paused/offscreen
		frame((performance.now() / 1000) * effSpeed);

		// pause offscreen + on hidden tabs — free when not visible
		let visible = true;
		const io =
			typeof IntersectionObserver !== 'undefined'
				? new IntersectionObserver(([entry]) => {
						visible = entry.isIntersecting;
						if (visible && document.visibilityState !== 'hidden') start();
						else stop();
					})
				: null;
		io?.observe(canvas);
		const onVis = () => {
			if (document.visibilityState === 'hidden') stop();
			else if (visible) start();
		};
		document.addEventListener('visibilitychange', onVis);
		if (!io) start();

		return () => {
			stop();
			io?.disconnect();
			document.removeEventListener('visibilitychange', onVis);
		};
	});
</script>

<canvas
	{...rest}
	bind:this={canvas}
	role="img"
	aria-label={ariaLabel ?? LABELS[orbState]}
	style:width="{size}px"
	style:height="{size}px"
	style:display="block"
></canvas>
