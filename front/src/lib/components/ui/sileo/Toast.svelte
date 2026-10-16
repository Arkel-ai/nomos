<script lang="ts">
	import { onMount, untrack } from "svelte";
	import {
		BLUR_RATIO,
		DEFAULT_ROUNDNESS,
		HEADER_EXIT_MS,
		HEIGHT,
		MIN_EXPAND_RATIO,
		PILL_PADDING,
		SWAP_COLLAPSE_MS,
		WIDTH,
	} from "./constants";
	import type { Snippet } from "svelte";
	import type { SileoButton, SileoState, SileoStyles } from "./types";

	/* ---------------------------------- Props --------------------------------- */

	interface Props {
		id: string;
		fill?: string;
		state?: SileoState;
		title?: string;
		description?: string;
		content?: Snippet;
		position?: "left" | "center" | "right";
		expand?: "top" | "bottom";
		icon?: null;
		styles?: SileoStyles;
		button?: SileoButton;
		roundness?: number;
		exiting?: boolean;
		autoExpandDelayMs?: number;
		autoCollapseDelayMs?: number;
		canExpand?: boolean;
		refreshKey?: string;
		onMouseEnter?: (e: MouseEvent) => void;
		onMouseLeave?: (e: MouseEvent) => void;
		onDismiss?: () => void;
	}

	let {
		id,
		fill,
		state: toastState = "success",
		title = toastState,
		description,
		content,
		position = "left",
		expand = "bottom",
		icon,
		styles,
		button,
		roundness,
		exiting = false,
		autoExpandDelayMs,
		autoCollapseDelayMs,
		canExpand,
		refreshKey,
		onMouseEnter,
		onMouseLeave,
		onDismiss,
	}: Props = $props();

	/* ---------------------------------- View ---------------------------------- */

	interface View {
		title?: string;
		description?: string;
		content?: Snippet;
		state: SileoState;
		icon?: null;
		styles?: SileoStyles;
		button?: SileoButton;
		fill?: string;
	}

	let view = $state<View>(
		untrack(() => ({
			title,
			description,
			content,
			state: toastState,
			icon,
			styles,
			button,
			fill,
		})),
	);
	let applied = $state(untrack(() => refreshKey));
	let isExpanded = $state(false);
	let ready = $state(false);
	let pillWidth = $state(0);
	let contentHeight = $state(0);

	let hasDesc = $derived(Boolean(view.description) || Boolean(view.content) || Boolean(view.button));
	let isLoading = $derived(view.state === "loading");
	let open = $derived(hasDesc && isExpanded && !isLoading);
	let allowExpand = $derived(isLoading ? false : (canExpand ?? true));

	let headerKey = $derived(`${view.state}-${view.title}`);
	let filterId = $derived(`sileo-gooey-${id}`);
	let resolvedRoundness = $derived(Math.max(0, roundness ?? DEFAULT_ROUNDNESS));
	let blur = $derived(resolvedRoundness * BLUR_RATIO);

	/* ---------------------------------- Refs ---------------------------------- */

	let buttonEl: HTMLButtonElement | undefined = $state();
	let headerEl: HTMLDivElement | undefined = $state();
	let innerEl: HTMLDivElement | undefined = $state();
	let contentEl: HTMLDivElement | undefined = $state();

	let headerExitTimer: number | null = null;
	let autoExpandTimer: number | null = null;
	let autoCollapseTimer: number | null = null;
	let swapTimer: number | null = null;
	let lastRefreshKey = untrack(() => refreshKey);
	let pending: { key?: string; payload: View } | null = null;

	/* ----------------------------- Header Layers ----------------------------- */

	interface HeaderLayer {
		current: { key: string; view: View };
		prev: { key: string; view: View } | null;
	}

	let headerLayer = $state<HeaderLayer>(
		untrack(() => ({
			current: { key: headerKey, view },
			prev: null,
		})),
	);

	/* ------------------------------ Measurements ----------------------------- */

	let headerPad: number | null = null;
	let pillRo: ResizeObserver | null = null;
	let pillRafId = 0;
	let pillObserved: Element | null = null;

	function measurePillWidth() {
		const el = innerEl;
		const header = headerEl;
		if (!el || !header) return;
		if (headerPad === null) {
			const cs = getComputedStyle(header);
			headerPad = parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight);
		}
		const w = el.scrollWidth + headerPad + PILL_PADDING;
		if (w > PILL_PADDING) {
			pillWidth = w;
		}
	}

	// Observe innerEl for pill width changes
	$effect(() => {
		// Track headerLayer.current.key to re-measure when header changes
		const _key = headerLayer.current.key;
		void _key;

		measurePillWidth();

		if (!pillRo) {
			pillRo = new ResizeObserver(() => {
				cancelAnimationFrame(pillRafId);
				pillRafId = requestAnimationFrame(() => {
					const inner = innerEl;
					const pad = headerPad ?? 0;
					if (!inner) return;
					const w = inner.scrollWidth + pad + PILL_PADDING;
					if (w > PILL_PADDING) {
						pillWidth = w;
					}
				});
			});
		}

		const el = innerEl;
		if (el && pillObserved !== el) {
			if (pillObserved) {
				pillRo.unobserve(pillObserved);
			}
			pillRo.observe(el);
			pillObserved = el;
		}

		return () => {
			cancelAnimationFrame(pillRafId);
			pillRo?.disconnect();
			pillRo = null;
			pillObserved = null;
		};
	});

	// Content height measurement
	$effect(() => {
		if (!hasDesc) {
			contentHeight = 0;
			return;
		}
		const el = contentEl;
		if (!el) return;
		const measure = () => {
			const h = el.scrollHeight;
			contentHeight = h;
		};
		measure();
		let rafId = 0;
		const ro = new ResizeObserver(() => {
			cancelAnimationFrame(rafId);
			rafId = requestAnimationFrame(measure);
		});
		ro.observe(el);
		return () => {
			cancelAnimationFrame(rafId);
			ro.disconnect();
		};
	});

	// Ready state via rAF
	onMount(() => {
		const raf = requestAnimationFrame(() => {
			ready = true;
		});
		return () => cancelAnimationFrame(raf);
	});

	/* ----------------------------- Header Layer Sync ------------------------- */

	$effect(() => {
		const hk = headerKey;
		const v = view;
		if (headerLayer.current.key === hk) {
			if (headerLayer.current.view !== v) {
				headerLayer = { ...headerLayer, current: { key: hk, view: v } };
			}
		} else {
			headerLayer = {
				prev: headerLayer.current,
				current: { key: hk, view: v },
			};
		}
	});

	// Clear prev after exit animation
	$effect(() => {
		if (!headerLayer.prev) return;
		if (headerExitTimer) {
			clearTimeout(headerExitTimer);
		}
		headerExitTimer = window.setTimeout(() => {
			headerExitTimer = null;
			headerLayer = { ...headerLayer, prev: null };
		}, HEADER_EXIT_MS);
		return () => {
			if (headerExitTimer) {
				clearTimeout(headerExitTimer);
				headerExitTimer = null;
			}
		};
	});

	/* -------------------------------- Fill sync ------------------------------- */

	$effect(() => {
		if (view.fill !== fill) {
			view = { ...view, fill };
		}
	});

	/* ------------------------------ Refresh logic ----------------------------- */

	$effect(() => {
		const next: View = { title, description, content, state: toastState, icon, styles, button, fill };

		if (refreshKey === undefined) {
			view = next;
			applied = undefined;
			pending = null;
			lastRefreshKey = refreshKey;
			return;
		}

		if (lastRefreshKey === refreshKey) return;
		lastRefreshKey = refreshKey;

		if (swapTimer) {
			clearTimeout(swapTimer);
			swapTimer = null;
		}

		if (open) {
			pending = { key: refreshKey, payload: next };
			isExpanded = false;
			swapTimer = window.setTimeout(() => {
				swapTimer = null;
				const p = pending;
				if (!p) return;
				view = p.payload;
				applied = p.key;
				pending = null;
			}, SWAP_COLLAPSE_MS);
		} else {
			pending = null;
			view = next;
			applied = refreshKey;
		}
	});

	/* ----------------------------- Auto expand/collapse ----------------------- */

	$effect(() => {
		if (!hasDesc) return;

		if (autoExpandTimer) clearTimeout(autoExpandTimer);
		if (autoCollapseTimer) clearTimeout(autoCollapseTimer);

		if (exiting || !allowExpand) {
			isExpanded = false;
			return;
		}

		if (autoExpandDelayMs == null && autoCollapseDelayMs == null) return;

		// Track applied to re-run when content refreshes
		void applied;

		const expandDelay = autoExpandDelayMs ?? 0;
		const collapseDelay = autoCollapseDelayMs ?? 0;

		if (expandDelay > 0) {
			autoExpandTimer = window.setTimeout(
				() => { isExpanded = true; },
				expandDelay,
			);
		} else {
			isExpanded = true;
		}

		if (collapseDelay > 0) {
			autoCollapseTimer = window.setTimeout(
				() => { isExpanded = false; },
				collapseDelay,
			);
		}

		return () => {
			if (autoExpandTimer) clearTimeout(autoExpandTimer);
			if (autoCollapseTimer) clearTimeout(autoCollapseTimer);
		};
	});

	/* ------------------------------ Derived values ---------------------------- */

	let minExpanded = $derived(HEIGHT * MIN_EXPAND_RATIO);
	let rawExpanded = $derived(
		hasDesc ? Math.max(minExpanded, HEIGHT + contentHeight) : minExpanded,
	);

	let frozenExpanded = untrack(() => rawExpanded);
	$effect(() => {
		if (open) {
			frozenExpanded = rawExpanded;
		}
	});

	let expanded = $derived(open ? rawExpanded : frozenExpanded);
	let svgHeight = $derived(hasDesc ? Math.max(expanded, minExpanded) : HEIGHT);
	let expandedContent = $derived(Math.max(0, expanded - HEIGHT));
	let resolvedPillWidth = $derived(Math.max(pillWidth || HEIGHT, HEIGHT));
	let pillHeight = $derived(HEIGHT + blur * 3);

	let pillX = $derived(
		position === "right"
			? WIDTH - resolvedPillWidth
			: position === "center"
				? (WIDTH - resolvedPillWidth) / 2
				: 0,
	);

	/* ------------------------------- SVG values ------------------------------ */

	let pillStyle = $derived(
		`x: ${pillX}px; width: ${resolvedPillWidth}px; height: ${open ? pillHeight : HEIGHT}px`,
	);
	let bodyHeight = $derived(open ? expandedContent : 0);
	let bodyOpacity = $derived(open ? 1 : 0);

	let viewBox = $derived(`0 0 ${WIDTH} ${svgHeight}`);
	let canvasStyle = $derived(`filter: url(#${filterId})`);

	/* ------------------------------- Root style ------------------------------- */

	let rootStyle = $derived(
		`--_h: ${open ? expanded : HEIGHT}px; --_pw: ${resolvedPillWidth}px; --_px: ${pillX}px; --_ht: translateY(${open ? (expand === "bottom" ? 3 : -3) : 0}px) scale(${open ? 0.9 : 1}); --_co: ${open ? 1 : 0}${view.fill ? `; --sileo-fill: ${view.fill}` : ""}`,
	);

	/* -------------------------------- Handlers -------------------------------- */

	function handleEnter(e: MouseEvent) {
		onMouseEnter?.(e);
		if (hasDesc) isExpanded = true;
	}

	function handleLeave(e: MouseEvent) {
		onMouseLeave?.(e);
		isExpanded = false;
	}

	function handleTransitionEnd(e: TransitionEvent) {
		if (e.propertyName !== "height" && e.propertyName !== "transform") return;
		if (open) return;
		const p = pending;
		if (!p) return;
		if (swapTimer) {
			clearTimeout(swapTimer);
			swapTimer = null;
		}
		view = p.payload;
		applied = p.key;
		pending = null;
	}

	/* -------------------------------- Swipe ----------------------------------- */

	const SWIPE_DISMISS = 30;
	const SWIPE_MAX = 20;
	let pointerStart: number | null = null;

	function onSwipeMove(e: PointerEvent) {
		const el = buttonEl;
		if (pointerStart === null || !el) return;
		const dy = e.clientY - pointerStart;
		const sign = dy > 0 ? 1 : -1;
		const clamped = Math.min(Math.abs(dy), SWIPE_MAX) * sign;
		el.style.transform = `translateY(${clamped}px)`;
	}

	function onSwipeUp(e: PointerEvent) {
		const el = buttonEl;
		if (pointerStart === null || !el) return;
		const dy = e.clientY - pointerStart;
		pointerStart = null;
		el.style.transform = "";
		el.removeEventListener("pointermove", onSwipeMove);
		el.removeEventListener("pointerup", onSwipeUp);
		if (Math.abs(dy) > SWIPE_DISMISS) {
			onDismiss?.();
		}
	}

	function handlePointerDown(e: PointerEvent) {
		if (exiting || !onDismiss) return;
		const target = e.target as HTMLElement;
		if (target.closest("[data-sileo-button]")) return;
		if (target.closest("a, button, input, textarea, select, [role='button'], [data-sileo-description]")) return;
		pointerStart = e.clientY;
		(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
		const el = buttonEl;
		if (el) {
			el.addEventListener("pointermove", onSwipeMove, { passive: true });
			el.addEventListener("pointerup", onSwipeUp, { passive: true });
		}
	}

	function handleButtonClick(e: MouseEvent) {
		e.preventDefault();
		e.stopPropagation();
		view.button?.onClick();
	}
</script>

<button
	bind:this={buttonEl}
	type="button"
	data-sileo-toast
	data-ready={ready}
	data-expanded={open}
	data-exiting={exiting}
	data-edge={expand}
	data-position={position}
	data-state={view.state}
	style={rootStyle}
	onmouseenter={handleEnter}
	onmouseleave={handleLeave}
	ontransitionend={handleTransitionEnd}
	onpointerdown={handlePointerDown}
>
	<!-- SVG Canvas -->
	<div data-sileo-canvas data-edge={expand} style={canvasStyle}>
		<svg data-sileo-svg width={WIDTH} height={svgHeight} viewBox={viewBox}>
			<title>Sileo Notification</title>
			<defs>
				<filter
					id={filterId}
					x="-20%"
					y="-20%"
					width="140%"
					height="140%"
					color-interpolation-filters="sRGB"
				>
					<feGaussianBlur in="SourceGraphic" stdDeviation={blur} result="blur" />
					<feColorMatrix
						in="blur"
						mode="matrix"
						values="1 0 0 0 0  0 1 0 0 0  0 0 1 0 0  0 0 0 20 -10"
						result="goo"
					/>
					<feComposite in="SourceGraphic" in2="goo" operator="atop" />
				</filter>
			</defs>

			<!-- Pill -->
			<rect
				data-sileo-pill
				rx={resolvedRoundness}
				ry={resolvedRoundness}
				y="0"
				style={pillStyle}
			/>

			<!-- Body -->
			<rect
				data-sileo-body
				y={HEIGHT}
				width={WIDTH}
				rx={resolvedRoundness}
				ry={resolvedRoundness}
				style="height: {bodyHeight}px; opacity: {bodyOpacity}"
			/>
		</svg>
	</div>

	<!-- Header -->
	<div bind:this={headerEl} data-sileo-header data-edge={expand}>
		<div data-sileo-header-stack>
			<!-- Current layer -->
			<div
				bind:this={innerEl}
				data-sileo-header-inner
				data-layer="current"
			>
				<div
					data-sileo-badge
					data-state={headerLayer.current.view.state}
					class={headerLayer.current.view.styles?.badge}
				>
					{#if headerLayer.current.view.state === "success"}
						<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Check</title><path d="M20 6 9 17l-5-5" /></svg>
					{:else if headerLayer.current.view.state === "loading"}
						<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" data-sileo-icon="spin" aria-hidden="true"><title>Loader</title><path d="M21 12a9 9 0 1 1-6.219-8.56" /></svg>
					{:else if headerLayer.current.view.state === "error"}
						<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>X</title><path d="M18 6 6 18" /><path d="m6 6 12 12" /></svg>
					{:else if headerLayer.current.view.state === "warning"}
						<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Alert</title><circle cx="12" cy="12" r="10" /><line x1="12" x2="12" y1="8" y2="12" /><line x1="12" x2="12.01" y1="16" y2="16" /></svg>
					{:else if headerLayer.current.view.state === "info"}
						<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Info</title><circle cx="12" cy="12" r="10" /><path d="m4.93 4.93 4.24 4.24" /><path d="m14.83 9.17 4.24-4.24" /><path d="m14.83 14.83 4.24 4.24" /><path d="m9.17 14.83-4.24 4.24" /><circle cx="12" cy="12" r="4" /></svg>
					{:else if headerLayer.current.view.state === "action"}
						<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Arrow</title><path d="M5 12h14" /><path d="m12 5 7 7-7 7" /></svg>
					{/if}
				</div>
				<span
					data-sileo-title
					data-state={headerLayer.current.view.state}
					class={headerLayer.current.view.styles?.title}
				>
					{headerLayer.current.view.title}
				</span>
			</div>

			<!-- Previous layer (exiting) -->
			{#if headerLayer.prev}
				<div
					data-sileo-header-inner
					data-layer="prev"
					data-exiting="true"
				>
					<div
						data-sileo-badge
						data-state={headerLayer.prev.view.state}
						class={headerLayer.prev.view.styles?.badge}
					>
						{#if headerLayer.prev.view.state === "success"}
							<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Check</title><path d="M20 6 9 17l-5-5" /></svg>
						{:else if headerLayer.prev.view.state === "loading"}
							<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" data-sileo-icon="spin" aria-hidden="true"><title>Loader</title><path d="M21 12a9 9 0 1 1-6.219-8.56" /></svg>
						{:else if headerLayer.prev.view.state === "error"}
							<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>X</title><path d="M18 6 6 18" /><path d="m6 6 12 12" /></svg>
						{:else if headerLayer.prev.view.state === "warning"}
							<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Alert</title><circle cx="12" cy="12" r="10" /><line x1="12" x2="12" y1="8" y2="12" /><line x1="12" x2="12.01" y1="16" y2="16" /></svg>
						{:else if headerLayer.prev.view.state === "info"}
							<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Info</title><circle cx="12" cy="12" r="10" /><path d="m4.93 4.93 4.24 4.24" /><path d="m14.83 9.17 4.24-4.24" /><path d="m14.83 14.83 4.24 4.24" /><path d="m9.17 14.83-4.24 4.24" /><circle cx="12" cy="12" r="4" /></svg>
						{:else if headerLayer.prev.view.state === "action"}
							<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><title>Arrow</title><path d="M5 12h14" /><path d="m12 5 7 7-7 7" /></svg>
						{/if}
					</div>
					<span
						data-sileo-title
						data-state={headerLayer.prev.view.state}
						class={headerLayer.prev.view.styles?.title}
					>
						{headerLayer.prev.view.title}
					</span>
				</div>
			{/if}
		</div>
	</div>

	<!-- Content -->
	{#if hasDesc}
		<div data-sileo-content data-edge={expand} data-visible={open}>
			<div
				bind:this={contentEl}
				data-sileo-description
				class={view.styles?.description}
			>
				{#if view.content}
					{@render view.content()}
				{:else}
					{view.description}
				{/if}
				{#if view.button}
					<!-- svelte-ignore a11y_invalid_attribute -->
					<a
						href="#"
						data-sileo-button
						data-state={view.state}
						class={view.styles?.button}
						onclick={handleButtonClick}
					>
						{view.button.title}
					</a>
				{/if}
			</div>
		</div>
	{/if}
</button>
