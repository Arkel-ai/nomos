<script lang="ts">
	import { onDestroy } from "svelte";
	import { mode } from "mode-watcher";
	import { store, timeoutKey } from "./store.svelte";
	import { DEFAULT_TOAST_DURATION } from "./constants";
	import type { SileoItem, SileoOptions, SileoPosition } from "./types";
	import Toast from "./Toast.svelte";
	import "./sileo.css";

	/* ---------------------------------- Props --------------------------------- */

	interface Props {
		position?: SileoPosition;
		options?: Partial<SileoOptions>;
		theme?: "light" | "dark" | "system";
	}

	let {
		position = "top-right",
		options,
		theme,
	}: Props = $props();

	/* -------------------------------- Constants ------------------------------- */

	const pillAlign = (pos: SileoPosition) =>
		pos.includes("right") ? "right" : pos.includes("center") ? "center" : "left";

	const expandDir = (pos: SileoPosition) =>
		pos.startsWith("top") ? ("bottom" as const) : ("top" as const);

	/* ---------------------------------- State --------------------------------- */

	let activeId = $state<string | undefined>(undefined);
	let hovering = false;
	let timers = new Map<string, number>();

	/* ----------------------------- Theme resolution --------------------------- */

	let resolvedTheme = $derived.by((): "light" | "dark" => {
		if (theme === "light" || theme === "dark") return theme;
		// Use mode-watcher for system theme
		const m = mode.current;
		if (m === "light" || m === "dark") return m;
		// Fallback
		if (typeof window !== "undefined") {
			return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
		}
		return "light";
	});

	/* ----------------------------- Sync store config -------------------------- */

	$effect(() => {
		store.position = position;
		store.options = options;
	});

	/* ----------------------------- Timer management --------------------------- */

	function clearAllTimers() {
		for (const t of timers.values()) clearTimeout(t);
		timers.clear();
	}

	function schedule(items: SileoItem[]) {
		if (hovering) return;

		for (const item of items) {
			if (item.exiting) continue;
			const key = timeoutKey(item);
			if (timers.has(key)) continue;

			if (item.duration === null) continue;
			const dur = item.duration ?? DEFAULT_TOAST_DURATION;
			if (dur <= 0) continue;

			timers.set(
				key,
				window.setTimeout(() => store.dismiss(item.id), dur),
			);
		}
	}

	// React to toast changes: clean stale timers and schedule new ones
	$effect(() => {
		const toasts = store.toasts;

		const toastKeys = new Set(toasts.map(timeoutKey));
		const toastIds = new Set(toasts.map((t) => t.id));

		for (const [key, timer] of timers) {
			if (!toastKeys.has(key)) {
				clearTimeout(timer);
				timers.delete(key);
			}
		}

		schedule(toasts);
	});

	onDestroy(clearAllTimers);

	/* -------------------------------- Latest ID ------------------------------ */

	let latest = $derived.by(() => {
		const toasts = store.toasts;
		for (let i = toasts.length - 1; i >= 0; i--) {
			if (!toasts[i].exiting) return toasts[i].id;
		}
		return undefined;
	});

	$effect(() => {
		activeId = latest;
	});

	/* ----------------------------- Position grouping -------------------------- */

	let activePositions = $derived.by(() => {
		const map = new Map<SileoPosition, SileoItem[]>();
		for (const t of store.toasts) {
			const pos = t.position ?? position;
			const arr = map.get(pos);
			if (arr) {
				arr.push(t);
			} else {
				map.set(pos, [t]);
			}
		}
		return map;
	});

	/* -------------------------------- Handlers -------------------------------- */

	function handleMouseEnter(toastId: string) {
		return (_e: MouseEvent) => {
			if (activeId !== toastId) activeId = toastId;
			if (!hovering) {
				hovering = true;
				clearAllTimers();
			}
		};
	}

	function handleMouseLeave(_toastId: string) {
		return (_e: MouseEvent) => {
			activeId = latest;
			if (hovering) {
				hovering = false;
				schedule(store.toasts);
			}
		};
	}

	function handleDismiss(toastId: string) {
		return () => store.dismiss(toastId);
	}
</script>

{#each [...activePositions] as [pos, items] (pos)}
	{@const pill = pillAlign(pos)}
	{@const expand = expandDir(pos)}
	<section
		data-sileo-viewport
		data-position={pos}
		data-theme={theme ? resolvedTheme : undefined}
		aria-live="polite"
	>
		{#each items as item (item.id)}
			<Toast
				id={item.id}
				state={item.state}
				title={item.title}
				description={item.description}
				content={item.content}
				position={pill}
				{expand}
				icon={item.icon}
				fill={item.fill}
				styles={item.styles}
				button={item.button}
				roundness={item.roundness}
				exiting={item.exiting}
				autoExpandDelayMs={item.autoExpandDelayMs}
				autoCollapseDelayMs={item.autoCollapseDelayMs}
				refreshKey={item.instanceId}
				canExpand={activeId === undefined || activeId === item.id}
				onMouseEnter={handleMouseEnter(item.id)}
				onMouseLeave={handleMouseLeave(item.id)}
				onDismiss={handleDismiss(item.id)}
			/>
		{/each}
	</section>
{/each}
