import { fade, fly, type FlyParams, type TransitionConfig } from 'svelte/transition';
import { flip as svelteFlip, type FlipParams } from 'svelte/animate';
import { cubicOut } from 'svelte/easing';

export const motionQuery = '(prefers-reduced-motion: reduce)';
export const reducedMotion = () =>
	typeof window !== 'undefined' && window.matchMedia(motionQuery).matches;

// A lightly damped spring, with an exact endpoint for Svelte transitions.
export const spring = (t: number) =>
	t === 0 || t === 1 ? t : 1 - Math.exp(-8 * t) * Math.cos(10 * t);

/** State changes: preserve the element's existing transform, shorten exits. */
export function enter(
	node: Element,
	params: FlyParams = {},
	options?: { direction: 'in' | 'out' | 'both' }
): TransitionConfig {
	if (reducedMotion()) return fade(node, { duration: 100 });
	return fly(node, {
		y: 8,
		duration: options?.direction === 'out' ? 140 : 320,
		easing: spring,
		...params,
		delay: Math.min(params.delay ?? 0, 240)
	});
}

/** Keyed list movement, with no spatial motion in the reduced-motion mode. */
export function flip(node: Element, rects: { from: DOMRect; to: DOMRect }, params: FlipParams = {}) {
	return svelteFlip(node, rects, {
		easing: cubicOut,
		...params,
		duration: reducedMotion() ? 0 : 260
	});
}

type RevealOptions = { delay?: number; y?: number; duration?: number };

/** One-shot, viewport-triggered reveal. Content is visible without JS and on focus. */
export function reveal(node: HTMLElement, options: RevealOptions = {}) {
	const preference = window.matchMedia(motionQuery);
	let animation: Animation | undefined;
	let observer: IntersectionObserver | undefined;
	let revealed = false;

	function finish() {
		revealed = true;
		observer?.disconnect();
		animation?.cancel();
		delete node.dataset.revealPending;
	}

	function show() {
		if (revealed) return;
		revealed = true;
		observer?.disconnect();
		delete node.dataset.revealPending;
		if (preference.matches) return;
		const easing = getComputedStyle(node).getPropertyValue('--motion-spring').trim();
		animation = node.animate(
			[{ opacity: 0, translate: `0 ${options.y ?? 12}px` }, { opacity: 1, translate: '0 0' }],
			{
				duration: options.duration ?? 440,
				delay: Math.min(options.delay ?? 0, 240),
				easing: easing || 'cubic-bezier(0.16, 1, 0.3, 1)',
				fill: 'backwards'
			}
		);
	}

	if (!preference.matches && 'IntersectionObserver' in window) {
		node.dataset.revealPending = '';
		// Threshold zero also works for a section taller than the viewport.
		observer = new IntersectionObserver((entries) => {
			if (entries.some((entry) => entry.isIntersecting)) show();
		}, { threshold: 0 });
		observer.observe(node);
	}

	function onPreferenceChange() {
		if (preference.matches) finish();
	}
	preference.addEventListener('change', onPreferenceChange);
	node.addEventListener('focusin', finish);
	return {
		destroy() {
			finish();
			preference.removeEventListener('change', onPreferenceChange);
			node.removeEventListener('focusin', finish);
		}
	};
}
