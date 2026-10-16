<script lang="ts">
	import { Tabs as TabsPrimitive } from "bits-ui";
	import { cn } from "$lib/utils.js";

	let {
		ref = $bindable(null),
		class: className,
		...restProps
	}: TabsPrimitive.TriggerProps = $props();
</script>

<TabsPrimitive.Trigger
	bind:ref
	data-slot="tabs-trigger"
	class={cn(
		"focus-visible:border-ring focus-visible:ring-ring/50 data-[state=active]:bg-primary data-[state=active]:text-accent text-muted-foreground inline-flex h-[calc(100%-1px)] flex-1 items-center justify-center gap-0.5 whitespace-nowrap rounded-lg border border-transparent px-2 py-1 text-sm font-medium transition-[color,box-shadow] disabled:pointer-events-none disabled:opacity-50 data-[state=active]:shadow-sm [&_svg:not([class*='size-'])]:size-4 [&_svg]:pointer-events-none [&_svg]:shrink-0",
		className
	)}
	{...restProps}
/>
