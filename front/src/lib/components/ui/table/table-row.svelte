<script lang="ts">
	import { cn, type WithElementRef } from "$lib/utils.js";
	import type { HTMLAttributes } from "svelte/elements";

	let {
		ref = $bindable(null),
		class: className,
		children,
		...restProps
	}: WithElementRef<HTMLAttributes<HTMLTableRowElement>> = $props();
</script>

<tr bind:this={ref} data-slot="table-row" class={cn("hover:bg-muted/40 data-[state=selected]:bg-accent/40 border-b border-border/60 transition-colors duration-150", className)} {...restProps}>
	{@render children?.()}
</tr>
