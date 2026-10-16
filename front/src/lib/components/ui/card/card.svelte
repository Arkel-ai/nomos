<script lang="ts" module>
	import { tv, type VariantProps } from "tailwind-variants";

	export const cardVariants = tv({
		base: "flex flex-col gap-4 rounded-xl border p-4",
		variants: {
			variant: {
				default:
					"bg-card text-card-foreground shadow",
				primary:
					"bg-gradient-to-b from-primary/80 to-primary dark:from-primary dark:to-primary/80 text-primary-foreground border-0 shadow-[inset_0_1px_0_rgba(255,255,255,0.22),0_1px_3px_0_rgba(0,0,0,0.08)] [&_[data-slot=card-description]]:text-white/70 [&_[data-slot=card-title]]:text-white",
			},
		},
		defaultVariants: {
			variant: "default",
		},
	});

	export type CardVariant = VariantProps<typeof cardVariants>["variant"];
</script>

<script lang="ts">
	import type { HTMLAttributes } from "svelte/elements";
	import { cn, type WithElementRef } from "$lib/utils.js";

	let {
		ref = $bindable(null),
		class: className,
		variant = "default",
		children,
		...restProps
	}: WithElementRef<HTMLAttributes<HTMLDivElement>> & {
		variant?: CardVariant;
	} = $props();
</script>

<div
	bind:this={ref}
	data-slot="card"
	class={cn(cardVariants({ variant }), className)}
	{...restProps}
>
	{@render children?.()}
</div>
