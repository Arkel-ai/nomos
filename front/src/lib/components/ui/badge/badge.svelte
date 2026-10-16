<script lang="ts" module>
	import { type VariantProps, tv } from "tailwind-variants";

	export const badgeVariants = tv({
		base: "focus-visible:border-ring focus-visible:ring-ring/50 aria-invalid:ring-destructive/20 dark:aria-invalid:ring-destructive/40 aria-invalid:border-destructive inline-flex w-fit shrink-0 items-center justify-center gap-1 overflow-hidden whitespace-nowrap rounded-lg border px-2 py-0.5 text-xs font-medium transition-[color,box-shadow] focus-visible:ring-[3px] [&>svg]:pointer-events-none [&>svg]:size-3",
		variants: {
			variant: {
				default:
					"bg-gradient-to-b from-primary/80 to-primary dark:from-primary dark:to-primary/80 text-primary-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.22)] [a&]:hover:from-primary/70 [a&]:hover:to-primary/90 dark:[a&]:hover:from-primary/90 dark:[a&]:hover:to-primary/70 border-0",
				secondary:
					"bg-gradient-to-b from-secondary/80 to-secondary dark:from-secondary dark:to-secondary/80 text-secondary-foreground shadow-[inset_0_1px_0_rgba(255,255,255,0.18)] [a&]:hover:from-secondary/70 [a&]:hover:to-secondary/90 dark:[a&]:hover:from-secondary/90 dark:[a&]:hover:to-secondary/70 border-transparent",
				destructive:
					"bg-gradient-to-b from-destructive/80 to-destructive dark:from-destructive dark:to-destructive/80 text-white shadow-[inset_0_1px_0_rgba(255,255,255,0.2)] [a&]:hover:from-destructive/70 [a&]:hover:to-destructive/90 dark:[a&]:hover:from-destructive/90 dark:[a&]:hover:to-destructive/70 focus-visible:ring-destructive/20 dark:focus-visible:ring-destructive/40 border-0",
				outline: "text-foreground [a&]:hover:bg-accent [a&]:hover:text-accent-foreground",
			},
		},
		defaultVariants: {
			variant: "default",
		},
	});

	export type BadgeVariant = VariantProps<typeof badgeVariants>["variant"];
</script>

<script lang="ts">
	import type { HTMLAnchorAttributes } from "svelte/elements";
	import { cn, type WithElementRef } from "$lib/utils.js";

	let {
		ref = $bindable(null),
		href,
		class: className,
		variant = "default",
		children,
		...restProps
	}: WithElementRef<HTMLAnchorAttributes> & {
		variant?: BadgeVariant;
	} = $props();
</script>

<svelte:element
	this={href ? "a" : "span"}
	bind:this={ref}
	data-slot="badge"
	{href}
	class={cn(badgeVariants({ variant }), className)}
	{...restProps}
>
	{@render children?.()}
</svelte:element>
