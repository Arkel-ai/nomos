import type { Snippet } from "svelte";

export type SileoState = "success" | "loading" | "error" | "warning" | "info" | "action";

export interface SileoStyles {
	title?: string;
	description?: string;
	badge?: string;
	button?: string;
}

export interface SileoButton {
	title: string;
	onClick: () => void;
}

export const SILEO_POSITIONS = [
	"top-left",
	"top-center",
	"top-right",
	"bottom-left",
	"bottom-center",
	"bottom-right",
] as const;

export type SileoPosition = (typeof SILEO_POSITIONS)[number];

export interface SileoOptions {
	id?: string;
	title?: string;
	description?: string;
	content?: Snippet;
	type?: SileoState;
	position?: SileoPosition;
	duration?: number | null;
	icon?: null;
	styles?: SileoStyles;
	fill?: string;
	roundness?: number;
	autopilot?: boolean | { expand?: number; collapse?: number };
	button?: SileoButton;
}

export interface InternalSileoOptions extends SileoOptions {
	state?: SileoState;
}

export interface SileoItem extends InternalSileoOptions {
	id: string;
	instanceId: string;
	exiting?: boolean;
	autoExpandDelayMs?: number;
	autoCollapseDelayMs?: number;
}

export interface SileoPromiseOptions<T = unknown> {
	loading: SileoOptions;
	success: SileoOptions | ((data: T) => SileoOptions);
	error: SileoOptions | ((err: unknown) => SileoOptions);
	action?: SileoOptions | ((data: T) => SileoOptions);
	position?: SileoPosition;
}
