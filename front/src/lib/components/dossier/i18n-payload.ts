/**
 * sveltekit-i18n is instantiated without a payload generic, so its `$t`
 * accepts only `{ default?: any }` and every interpolation object is an
 * excess-property error. Widening happens here rather than as a cast at each
 * call site. The real fix is a payload generic on the `new i18n(config)` in
 * `$lib/i18n` — that file is shared, so this stays local.
 */
export function tp<T extends Record<string, unknown>>(value: T): { default?: unknown } {
	return value as unknown as { default?: unknown };
}
