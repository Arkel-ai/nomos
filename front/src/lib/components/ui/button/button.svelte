<script lang="ts" module>
	import { cn, type WithElementRef } from "$lib/utils.js";
	import type { HTMLAnchorAttributes, HTMLButtonAttributes } from "svelte/elements";
	import { type VariantProps, tv } from "tailwind-variants";

	export const buttonVariants = tv({
		base: "focus-visible:border-ring focus-visible:ring-ring/50 aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40 aria-invalid:border-destructive inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-lg text-sm font-medium outline-none transition-all duration-150 ease-out focus-visible:ring-[3px] active:scale-[0.97] disabled:pointer-events-none disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50 [&_svg:not([class*='size-'])]:size-4 [&_svg]:pointer-events-none [&_svg]:shrink-0",
		variants: {
			variant: {
				default:
					"bg-gradient-to-b from-primary/80 to-primary dark:from-primary dark:to-primary/80 text-primary-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.25),0_1px_1px_-0.5px_rgba(0,0,0,0.06),0_3px_3px_-1.5px_rgba(0,0,0,0.06),0_6px_6px_-3px_rgba(0,0,0,0.06),0_12px_12px_-6px_rgba(0,0,0,0.06),0_24px_24px_-12px_rgba(0,0,0,0.06)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.25),0_1px_3px_0_rgba(0,0,0,0.05)] hover:from-primary/70 hover:to-primary/90 dark:hover:from-primary/90 dark:hover:to-primary/70 active:shadow-[inset_0_2px_4px_rgba(0,0,0,0.18)] active:from-primary/90 active:to-primary/90",
				destructive:
					"bg-gradient-to-b from-destructive/80 to-destructive dark:from-destructive dark:to-destructive/80 text-white shadow-[inset_0_1px_0_rgba(255,255,255,0.2),0_1px_1px_-0.5px_rgba(0,0,0,0.06),0_3px_3px_-1.5px_rgba(0,0,0,0.06),0_6px_6px_-3px_rgba(0,0,0,0.06),0_12px_12px_-6px_rgba(0,0,0,0.06),0_24px_24px_-12px_rgba(0,0,0,0.06)] dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.2),0_1px_3px_0_rgba(0,0,0,0.05)] hover:from-destructive/70 hover:to-destructive/90 dark:hover:from-destructive/90 dark:hover:to-destructive/70 active:shadow-[inset_0_2px_4px_rgba(0,0,0,0.2)] active:from-destructive/90 active:to-destructive/90 focus-visible:ring-destructive/20 dark:focus-visible:ring-destructive/40 dark:from-destructive/80 dark:to-destructive/70",
				outline:
					"bg-background shadow-xs hover:bg-accent hover:text-accent-foreground active:bg-accent/80 active:shadow-[inset_0_1px_2px_rgba(0,0,0,0.08)] dark:bg-input/30 dark:border-input dark:hover:bg-input/50 border border-sidebar-border",
				secondary:
					"bg-gradient-to-b from-secondary to-secondary/80 text-secondary-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.2),0_1px_3px_0_rgba(0,0,0,0.05)] hover:from-secondary/90 hover:to-secondary/70 active:shadow-[inset_0_2px_4px_rgba(0,0,0,0.1)] active:from-secondary/90 active:to-secondary/90",
				ghost: "hover:bg-accent hover:text-accent-foreground active:bg-accent/80 dark:hover:bg-accent/50",
				link: "text-primary underline-offset-4 hover:underline active:scale-100",
			},
			size: {
				default: "h-9 rounded-xl px-4 py-2 has-[>svg]:px-3",
				sm: "h-8 gap-1.5 rounded-xl px-3 has-[>svg]:px-2.5",
				lg: "h-10 rounded-xl px-6 has-[>svg]:px-4",
				icon: "size-9 rounded-xl",
			},
		},
		defaultVariants: {
			variant: "default",
			size: "default",
		},
	});

	export type ButtonVariant = VariantProps<typeof buttonVariants>["variant"];
	export type ButtonSize = VariantProps<typeof buttonVariants>["size"];

	export type ButtonProps = WithElementRef<HTMLButtonAttributes> &
		WithElementRef<HTMLAnchorAttributes> & {
			variant?: ButtonVariant;
			size?: ButtonSize;
		};
</script>

<script lang="ts">
	let {
		class: className,
		variant = "default",
		size = "default",
		ref = $bindable(null),
		href = undefined,
		type = "button",
		disabled,
		children,
		...restProps
	}: ButtonProps = $props();
</script>

{#if href}
	<a
		bind:this={ref}
		data-slot="button"
		class={cn(buttonVariants({ variant, size }), className)}
		href={disabled ? undefined : href}
		aria-disabled={disabled}
		role={disabled ? "link" : undefined}
		tabindex={disabled ? -1 : undefined}
		{...restProps}
	>
		{@render children?.()}
	</a>
{:else}
	<button
		bind:this={ref}
		data-slot="button"
		class={cn(buttonVariants({ variant, size }), className)}
		{type}
		{disabled}
		{...restProps}
	>
		{@render children?.()}
	</button>
{/if}
