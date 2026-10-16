import {
	AUTO_COLLAPSE_DELAY,
	AUTO_EXPAND_DELAY,
	DEFAULT_TOAST_DURATION,
	EXIT_DURATION,
} from "./constants";
import type {
	InternalSileoOptions,
	SileoItem,
	SileoOptions,
	SileoPosition,
} from "./types";

/* --------------------------------- Helpers -------------------------------- */

let idCounter = 0;
const generateId = () =>
	`${++idCounter}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;

export const timeoutKey = (t: SileoItem) => `${t.id}:${t.instanceId}`;

/* ------------------------------- Autopilot -------------------------------- */

const resolveAutopilot = (
	opts: InternalSileoOptions,
	duration: number | null,
): { expandDelayMs?: number; collapseDelayMs?: number } => {
	if (opts.autopilot === false || !duration || duration <= 0) return {};
	const cfg = typeof opts.autopilot === "object" ? opts.autopilot : undefined;
	const clamp = (v: number) => Math.min(duration, Math.max(0, v));
	return {
		expandDelayMs: clamp(cfg?.expand ?? AUTO_EXPAND_DELAY),
		collapseDelayMs: clamp(cfg?.collapse ?? AUTO_COLLAPSE_DELAY),
	};
};

/* --------------------------------- Store ---------------------------------- */

class SileoStore {
	toasts = $state<SileoItem[]>([]);
	position: SileoPosition = "top-right";
	options: Partial<SileoOptions> | undefined = undefined;

	private mergeOptions(options: InternalSileoOptions): InternalSileoOptions {
		return {
			...this.options,
			...options,
			styles: { ...this.options?.styles, ...options.styles },
		};
	}

	private buildItem(
		merged: InternalSileoOptions,
		id: string,
		fallbackPosition?: SileoPosition,
	): SileoItem {
		const duration = merged.duration ?? DEFAULT_TOAST_DURATION;
		const auto = resolveAutopilot(merged, duration);
		return {
			...merged,
			id,
			instanceId: generateId(),
			position: merged.position ?? fallbackPosition ?? this.position,
			autoExpandDelayMs: auto.expandDelayMs,
			autoCollapseDelayMs: auto.collapseDelayMs,
		};
	}

	create(options: InternalSileoOptions): { id: string; duration: number } {
		const live = this.toasts.filter((t) => !t.exiting);
		const merged = this.mergeOptions(options);

		const id = merged.id ?? generateId();
		const prev = live.find((t) => t.id === id);
		const item = this.buildItem(merged, id, prev?.position);

		if (prev) {
			this.toasts = this.toasts.map((t) => (t.id === id ? item : t));
		} else {
			this.toasts = [...this.toasts.filter((t) => t.id !== id), item];
		}
		return { id, duration: merged.duration ?? DEFAULT_TOAST_DURATION };
	}

	update(id: string, options: InternalSileoOptions): void {
		const existing = this.toasts.find((t) => t.id === id);
		if (!existing) return;

		const item = this.buildItem(this.mergeOptions(options), id, existing.position);
		this.toasts = this.toasts.map((t) => (t.id === id ? item : t));
	}

	dismiss(id: string): void {
		const item = this.toasts.find((t) => t.id === id);
		if (!item || item.exiting) return;

		this.toasts = this.toasts.map((t) =>
			t.id === id ? { ...t, exiting: true } : t,
		);

		setTimeout(() => {
			this.toasts = this.toasts.filter((t) => t.id !== id);
		}, EXIT_DURATION);
	}

	clear(position?: SileoPosition): void {
		if (position) {
			this.toasts = this.toasts.filter((t) => t.position !== position);
		} else {
			this.toasts = [];
		}
	}
}

export const store = new SileoStore();
