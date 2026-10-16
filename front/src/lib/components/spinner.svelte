<script lang="ts">
	// The one pending-action glyph: an arc turning on a faint track, in the
	// current text colour. Sits inside a button in place of its icon, so the
	// button keeps its width while the action runs.
	let { class: className = 'h-3.5 w-3.5' }: { class?: string } = $props();
</script>

<svg viewBox="0 0 16 16" fill="none" class="shrink-0 {className}" aria-hidden="true">
	<circle cx="8" cy="8" r="6" stroke="currentColor" stroke-opacity="0.22" stroke-width="2" />
	<path
		class="glyph-spin"
		d="M8 2a6 6 0 0 1 6 6"
		stroke="currentColor"
		stroke-width="2"
		stroke-linecap="round"
	/>
</svg>
