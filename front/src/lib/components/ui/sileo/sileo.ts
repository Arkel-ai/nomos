import { store } from "./store.svelte";
import type { SileoOptions, SileoPosition, SileoPromiseOptions } from "./types";

type Input = SileoOptions | string;

function normalize(input: Input): SileoOptions {
	return typeof input === "string" ? { title: input } : input;
}

export const sileo = {
	show(input: Input = {}): string {
		const opts = normalize(input);
		return store.create({ ...opts, state: opts.type }).id;
	},

	success(input: Input = {}): string {
		return store.create({ ...normalize(input), state: "success" }).id;
	},

	error(input: Input = {}): string {
		return store.create({ ...normalize(input), state: "error" }).id;
	},

	warning(input: Input = {}): string {
		return store.create({ ...normalize(input), state: "warning" }).id;
	},

	info(input: Input = {}): string {
		return store.create({ ...normalize(input), state: "info" }).id;
	},

	loading(input: Input = {}): string {
		return store.create({ ...normalize(input), state: "loading" }).id;
	},

	action(input: Input = {}): string {
		return store.create({ ...normalize(input), state: "action" }).id;
	},

	dismiss(id: string): void {
		store.dismiss(id);
	},

	clear(position?: SileoPosition): void {
		store.clear(position);
	},

	promise<T>(
		promise: Promise<T> | (() => Promise<T>),
		opts: SileoPromiseOptions<T>,
	): Promise<T> {
		const { id } = store.create({
			...opts.loading,
			state: "loading",
			duration: null,
			position: opts.position,
		});

		const p = typeof promise === "function" ? promise() : promise;

		p.then((data) => {
			if (opts.action) {
				const actionOpts =
					typeof opts.action === "function" ? opts.action(data) : opts.action;
				store.update(id, { ...actionOpts, state: "action", id });
			} else {
				const successOpts =
					typeof opts.success === "function"
						? opts.success(data)
						: opts.success;
				store.update(id, { ...successOpts, state: "success", id });
			}
		}).catch((err) => {
			const errorOpts =
				typeof opts.error === "function" ? opts.error(err) : opts.error;
			store.update(id, { ...errorOpts, state: "error", id });
		});

		return p;
	},
};
