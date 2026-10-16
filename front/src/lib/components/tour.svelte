<script lang="ts" module>
	import type { SolarIconName } from '$lib/icons/solar';

	export type TourStep = {
		/** CSS selector of the element to spotlight. Omit for a centered step. */
		target?: string;
		title: string;
		body: string;
		/** Medallion icon, shown on centered steps. */
		icon?: SolarIconName;
		/** Preferred side of the target for the card; flips when there's no room. */
		side?: 'top' | 'right' | 'bottom' | 'left';
	};
</script>

<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { Button } from '$lib/components/ui/button';
	import { t } from '$lib/i18n';
	import { reducedMotion } from '$lib/motion';
	import { tick } from 'svelte';
	import { fade } from 'svelte/transition';

	// Guided tour: dims the page, spotlights one element per step and anchors a
	// card beside it. A step without a `target` — or whose target isn't on
	// screen, e.g. the sidebar on mobile — renders as a centered card, so the
	// same steps work at every breakpoint. The page stays inert while it runs.
	let {
		steps,
		open = $bindable(false),
		onclose
	}: {
		steps: TourStep[];
		open?: boolean;
		/** Called when the tour ends; `completed` is false when it was skipped. */
		onclose?: (completed: boolean) => void;
	} = $props();

	const PAD = 6; // spotlight padding around the target
	const GAP = 12; // spotlight → card distance
	const MARGIN = 12; // card → viewport edge

	let index = $state(0);
	let rect = $state<DOMRect | null>(null);
	let card = $state<HTMLElement | null>(null);
	let cardW = $state(0);
	let cardH = $state(0);
	let vw = $state(0);
	let vh = $state(0);

	const step = $derived(steps[Math.min(index, steps.length - 1)]);
	const last = $derived(index >= steps.length - 1);

	// Reset to the first step on every open; hand focus to the card and give it
	// back to whatever had it when the tour ends.
	$effect(() => {
		if (!open) return;
		index = 0;
		const previous = document.activeElement as HTMLElement | null;
		tick().then(() => card?.focus());
		return () => previous?.focus?.();
	});

	// Track the target every frame while a step is shown: scrolling, sidebar
	// collapse and reveal animations all move it without firing an event. A
	// selector may match several elements (e.g. expanded and collapsed variants
	// of a control); the first visible one wins.
	$effect(() => {
		if (!open || !step) return;
		const els = step.target ? [...document.querySelectorAll<HTMLElement>(step.target)] : [];
		const visible = () =>
			els.map((el) => el.getBoundingClientRect()).find((r) => r.width > 0 && r.height > 0) ?? null;
		const first = visible();
		if (first && (first.top < 0 || first.bottom > window.innerHeight)) {
			els.find((el) => el.getBoundingClientRect().width > 0)?.scrollIntoView({
				block: 'center',
				behavior: reducedMotion() ? 'auto' : 'smooth'
			});
		}
		let frame = requestAnimationFrame(function measure() {
			const next = visible();
			if (!sameRect(rect, next)) rect = next;
			frame = requestAnimationFrame(measure);
		});
		return () => cancelAnimationFrame(frame);
	});

	function sameRect(a: DOMRect | null, b: DOMRect | null) {
		if (!a || !b) return a === b;
		return (
			Math.round(a.top) === Math.round(b.top) &&
			Math.round(a.left) === Math.round(b.left) &&
			Math.round(a.width) === Math.round(b.width) &&
			Math.round(a.height) === Math.round(b.height)
		);
	}

	const clamp = (v: number, min: number, max: number) => Math.max(min, Math.min(v, max));

	// The hole collapses to a point at the center on centered steps, so the
	// scrim covers everything and the spotlight grows out of it on the next one.
	const hole = $derived(
		rect
			? {
					top: rect.top - PAD,
					left: rect.left - PAD,
					width: rect.width + PAD * 2,
					height: rect.height + PAD * 2
				}
			: { top: vh / 2, left: vw / 2, width: 0, height: 0 }
	);

	const cardPos = $derived.by(() => {
		if (!rect) return { x: (vw - cardW) / 2, y: (vh - cardH) / 2 };
		const r = rect;
		const fits = {
			right: r.right + PAD + GAP + cardW <= vw - MARGIN,
			left: r.left - PAD - GAP - cardW >= MARGIN,
			bottom: r.bottom + PAD + GAP + cardH <= vh - MARGIN,
			top: r.top - PAD - GAP - cardH >= MARGIN
		};
		const order = [step.side ?? 'bottom', 'bottom', 'right', 'top', 'left'] as const;
		const side = order.find((s) => fits[s]) ?? 'bottom';
		let x: number;
		let y: number;
		if (side === 'left' || side === 'right') {
			x = side === 'right' ? r.right + PAD + GAP : r.left - PAD - GAP - cardW;
			y = r.top + r.height / 2 - cardH / 2;
		} else {
			x = r.left + r.width / 2 - cardW / 2;
			y = side === 'bottom' ? r.bottom + PAD + GAP : r.top - PAD - GAP - cardH;
		}
		return {
			x: clamp(x, MARGIN, vw - cardW - MARGIN),
			y: clamp(y, MARGIN, vh - cardH - MARGIN)
		};
	});

	function next() {
		if (last) finish(true);
		else index += 1;
	}

	function back() {
		if (index > 0) index -= 1;
	}

	function finish(completed: boolean) {
		open = false;
		onclose?.(completed);
	}

	function handleKey(e: KeyboardEvent) {
		if (!open) return;
		if (e.key === 'Escape') finish(false);
		else if (e.key === 'ArrowRight') next();
		else if (e.key === 'ArrowLeft') back();
		else return;
		e.preventDefault();
	}
</script>

<svelte:window bind:innerWidth={vw} bind:innerHeight={vh} onkeydown={handleKey} />

{#if open && step}
	<div class="fixed inset-0 z-[60]" transition:fade={{ duration: 150 }}>
		<div
			class="absolute rounded-xl shadow-[0_0_0_9999px_rgb(0_0_0/0.5)] ring-2 transition-all duration-300 ease-out motion-reduce:transition-none {rect
				? 'ring-primary/70'
				: 'ring-transparent'}"
			style="top: {hole.top}px; left: {hole.left}px; width: {hole.width}px; height: {hole.height}px;"
		></div>

		<div
			bind:this={card}
			bind:clientWidth={cardW}
			bind:clientHeight={cardH}
			role="dialog"
			aria-modal="true"
			aria-labelledby="tour-title"
			aria-describedby="tour-body"
			tabindex="-1"
			class="bg-popover text-popover-foreground border-border absolute w-[min(340px,calc(100vw-24px))] rounded-2xl border p-4 shadow-lg outline-none transition-[left,top] duration-300 ease-out motion-reduce:transition-none"
			style="left: {cardPos.x}px; top: {cardPos.y}px; visibility: {cardW ? 'visible' : 'hidden'};"
		>
			{#if !rect && step.icon}
				<div class="bg-primary/10 text-primary mb-3 flex h-10 w-10 items-center justify-center rounded-xl">
					<Icon name={step.icon} active class="h-5 w-5" />
				</div>
			{/if}

			<button
				type="button"
				onclick={() => finish(false)}
				aria-label={$t('tour.skip')}
				class="text-muted-foreground hover:text-foreground absolute right-3 top-3 cursor-pointer rounded-md p-1 transition-colors"
			>
				<Icon name="close-circle" class="h-4 w-4" />
			</button>
			<h3 id="tour-title" class="pr-6 text-[14px] font-semibold tracking-tight">{step.title}</h3>
			<p id="tour-body" class="text-muted-foreground mt-1 text-[12.5px] leading-relaxed">
				{step.body}
			</p>

			<div class="mt-4 flex items-center justify-between gap-3">
				<div
					class="flex items-center gap-1"
					role="progressbar"
					aria-valuemin={1}
					aria-valuemax={steps.length}
					aria-valuenow={index + 1}
				>
					{#if steps.length > 1}
						{#each steps as _, i}
							<span
								class="h-1.5 rounded-full transition-all duration-300 {i === index
									? 'bg-primary w-4'
									: 'bg-muted-foreground/25 w-1.5'}"
							></span>
						{/each}
					{/if}
				</div>
				<div class="flex items-center gap-1.5">
					{#if index > 0}
						<Button variant="ghost" size="sm" onclick={back}>{$t('common.back')}</Button>
					{:else if !last}
						<Button variant="ghost" size="sm" onclick={() => finish(false)}>
							{$t('tour.skip')}
						</Button>
					{/if}
					<Button size="sm" onclick={next}>{last ? $t('tour.done') : $t('common.next')}</Button>
				</div>
			</div>
		</div>
	</div>
{/if}
