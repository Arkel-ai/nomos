<script lang="ts">
	import { Slider as SliderPrimitive } from 'bits-ui';
	import * as Tooltip from '$lib/components/ui/tooltip';
	import { cn, type WithoutChildrenOrChild } from '$lib/utils.js';

	let {
		ref = $bindable(null),
		value = $bindable(),
		orientation = 'horizontal',
		class: className,
		...restProps
	}: WithoutChildrenOrChild<SliderPrimitive.RootProps> = $props();
	let tooltipRootOpen = $state(true);
</script>

<!--
Discriminated Unions + Destructing (required for bindable) do not
get along, so we shut typescript up by casting `value` to `never`.
-->
<SliderPrimitive.Root
	bind:ref
	bind:value={value as never}
	data-slot="slider"
	{orientation}
	class={cn(
		'relative flex w-full touch-none items-center select-none data-[disabled]:opacity-50 data-[orientation=vertical]:h-full data-[orientation=vertical]:min-h-44 data-[orientation=vertical]:w-auto data-[orientation=vertical]:flex-col',
		className
	)}
	{...restProps}
>
	{#snippet children({ thumbs })}
		<span
			data-orientation={orientation}
			data-slot="slider-track"
			class={cn(
				'bg-muted relative grow overflow-hidden rounded-full data-[orientation=horizontal]:h-1.5 data-[orientation=horizontal]:w-full data-[orientation=vertical]:h-full data-[orientation=vertical]:w-1.5'
			)}
		>
			<SliderPrimitive.Range
				data-slot="slider-range"
				class={cn(
					'bg-primary absolute data-[orientation=horizontal]:h-full data-[orientation=vertical]:w-full'
				)}
			/>
		</span>
		{#each thumbs as thumb (thumb)}
			<SliderPrimitive.Thumb
				data-slot="slider-thumb"
				index={thumb}
				class=""
			>
				<Tooltip.Provider delayDuration={0} disableCloseOnTriggerClick={false}>
					<Tooltip.Root>
						<Tooltip.Trigger>
							<div class="border-primary bg-background ring-ring/50 mt-1.5 block size-4 shrink-0 rounded-full border shadow-sm transition-[color,box-shadow] hover:ring-4 focus-visible:ring-4 focus-visible:outline-hidden disabled:pointer-events-none disabled:opacity-50">
							</div>
						</Tooltip.Trigger>
						<Tooltip.Content class="bg-card text-foreground" arrowClasses="bg-card">
							{value}
						</Tooltip.Content>
					</Tooltip.Root>
				</Tooltip.Provider>
			</SliderPrimitive.Thumb>
		{/each}
	{/snippet}
</SliderPrimitive.Root>
