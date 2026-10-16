<script lang="ts">
	import type { StepState } from '$lib/api/runs';

	/**
	 * The plan monitor's glyph: ○ queued, an Outremer arc turning inside a
	 * soft halo while running, a check that draws itself once done — and,
	 * deliberately unlike anything else, an amber paused disc pinging for
	 * `waiting_input`, because a run blocked on the lawyer is not a run that
	 * is working (docs/PLAN.md §4.4, §11).
	 *
	 * Colour comes from semantic tokens only, so both themes hold.
	 */

	let {
		state,
		class: className = ''
	}: { state: StepState | string; class?: string } = $props();
</script>

<span class="relative inline-flex h-4 w-4 shrink-0 items-center justify-center {className}">
	{#if state === 'running'}
		<!-- Turning, in its halo: the run is doing work right now. -->
		<span class="glyph-halo" aria-hidden="true"></span>
		<svg viewBox="0 0 16 16" fill="none" class="text-brand relative h-4 w-4" aria-hidden="true">
			<circle cx="8" cy="8" r="6.25" stroke="currentColor" stroke-opacity="0.22" stroke-width="1.5" />
			<path
				class="glyph-spin"
				d="M8 1.75 A6.25 6.25 0 0 1 14.25 8"
				stroke="currentColor"
				stroke-width="1.75"
				stroke-linecap="round"
			/>
		</svg>
	{:else if state === 'waiting_input'}
		<!-- Paused, not turning, and amber: the machine is waiting on a human. -->
		<span class="glyph-ping" aria-hidden="true"></span>
		<svg viewBox="0 0 16 16" class="text-amber relative h-4 w-4" aria-hidden="true">
			<circle cx="8" cy="8" r="6.25" fill="none" stroke="currentColor" stroke-width="1.5" />
			<rect x="6" y="5.25" width="1.4" height="5.5" rx="0.7" fill="currentColor" />
			<rect x="8.6" y="5.25" width="1.4" height="5.5" rx="0.7" fill="currentColor" />
		</svg>
	{:else if state === 'succeeded'}
		<svg viewBox="0 0 16 16" fill="none" class="text-pos h-4 w-4" aria-hidden="true">
			<circle cx="8" cy="8" r="6.25" fill="currentColor" fill-opacity="0.14" />
			<circle cx="8" cy="8" r="6.25" stroke="currentColor" stroke-width="1.5" />
			<path
				class="glyph-check"
				d="M5.2 8.3 L7.1 10.2 L10.9 5.9"
				stroke="currentColor"
				stroke-width="1.75"
				stroke-linecap="round"
				stroke-linejoin="round"
			/>
		</svg>
	{:else if state === 'failed'}
		<svg viewBox="0 0 16 16" fill="none" class="text-destructive h-4 w-4" aria-hidden="true">
			<circle cx="8" cy="8" r="6.25" fill="currentColor" fill-opacity="0.16" />
			<circle cx="8" cy="8" r="6.25" stroke="currentColor" stroke-width="1.5" />
			<path
				d="M5.6 5.6 L10.4 10.4 M10.4 5.6 L5.6 10.4"
				stroke="currentColor"
				stroke-width="1.75"
				stroke-linecap="round"
			/>
		</svg>
	{:else if state === 'cancelled'}
		<svg viewBox="0 0 16 16" fill="none" class="text-muted-foreground h-4 w-4" aria-hidden="true">
			<circle cx="8" cy="8" r="6.25" stroke="currentColor" stroke-width="1.5" />
			<path d="M3.9 12.1 L12.1 3.9" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
		</svg>
	{:else if state === 'skipped'}
		<svg viewBox="0 0 16 16" fill="none" class="text-muted-foreground/70 h-4 w-4" aria-hidden="true">
			<circle
				cx="8"
				cy="8"
				r="6.25"
				stroke="currentColor"
				stroke-width="1.5"
				stroke-dasharray="2.6 2.2"
			/>
			<path d="M5.4 8 L10.6 8" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
		</svg>
	{:else}
		<!-- queued, and anything a newer server invents: an open circle. -->
		<svg viewBox="0 0 16 16" fill="none" class="text-muted-foreground/50 h-4 w-4" aria-hidden="true">
			<circle cx="8" cy="8" r="6.25" stroke="currentColor" stroke-width="1.5" />
		</svg>
	{/if}
</span>
