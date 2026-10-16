<script lang="ts">
	import * as Tooltip from "$lib/components/ui/tooltip";
	export let progress
    export let tooltip: string = ""
	const angle = 360 * progress
	
	// Adapt the logic according to the approach
	const	background = `radial-gradient(var(--color-card) 58%, transparent 59%),
    conic-gradient(transparent 0deg ${angle}deg, var(--color-sidebar-border) ${angle}deg 360deg),
    conic-gradient(var(--color-primary-400) 0deg, var(--color-primary-800) 90deg, var(--color-primary-800) 180deg, var(--color-primary-800) 270deg);`;
	
	$: cssVarStyles = `background: ${background}`
</script>

<style>
#progress-circle {
  border-radius: 50%;
  width: 96px;
  height: 96px;
	transition: all 500ms ease-in;
	will-change: transform;
}
</style>
<Tooltip.Provider delayDuration={0}>
    <Tooltip.Root>
      <Tooltip.Trigger class="group flex items-center gap-2 cursor-pointer">
        <div class="flex items-center justify-center" id="progress-circle" style="{cssVarStyles}">
			<slot></slot>
		</div>
      </Tooltip.Trigger>
      <Tooltip.Content class="bg-card text-foreground" arrowClasses="bg-card">
        {tooltip}
      </Tooltip.Content>
    </Tooltip.Root>
  </Tooltip.Provider>
