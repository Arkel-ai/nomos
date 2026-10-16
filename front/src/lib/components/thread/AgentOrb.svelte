<script lang="ts">
	import { mode } from 'mode-watcher';
	import { ThinkingOrb, type OrbSize, type OrbState } from '$lib/components/libraries/thinking-orb';

	/**
	 * The machine at work, drawn. A thin wrapper over the starterkit's
	 * ThinkingOrb that takes its ink from the theme: Outremer while Nomos
	 * works, amber while it waits on the lawyer (the same amber as a paused
	 * plan step), and a still, muted frame once it is idle.
	 */

	let {
		state: orbState = 'working',
		tone = 'brand',
		size = 32,
		class: className = ''
	}: {
		state?: OrbState;
		tone?: 'brand' | 'amber' | 'idle';
		size?: OrbSize;
		class?: string;
	} = $props();

	// Resolve the token to a concrete colour (the canvas cannot read CSS
	// variables), and again whenever the theme flips.
	let color = $state<string | undefined>(undefined);
	$effect(() => {
		void mode.current;
		const token = tone === 'amber' ? '--amber' : tone === 'idle' ? '--muted-foreground' : '--brand';
		color = getComputedStyle(document.documentElement).getPropertyValue(token).trim() || undefined;
	});
</script>

<ThinkingOrb
	state={orbState}
	{size}
	{color}
	paused={tone === 'idle'}
	class={className}
	aria-hidden="true"
/>
