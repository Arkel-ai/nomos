<script lang="ts">
	// The Nomos mark: an ink tile carrying a geometric N, and one point of
	// signal light — the only colour the brand allows itself. Ink inverts with
	// the theme (near-white tile in dark mode), the dot does not.
	let { size = 24, class: className = '' }: { size?: number; class?: string } = $props();
	const id = `nm-${Math.random().toString(36).slice(2, 8)}`;
</script>

<svg
	viewBox="0 0 32 32"
	width={size}
	height={size}
	class="shrink-0 {className}"
	role="img"
	aria-label="Nomos"
>
	<defs>
		<linearGradient id={id} x1="0" y1="0" x2="1" y2="1">
			<stop offset="0" stop-color="var(--brand)" />
			<stop offset="1" stop-color="var(--signal)" />
		</linearGradient>
	</defs>
	<rect width="32" height="32" rx="9" fill="var(--foreground)" />
	<path
		d="M9 9h3.2l7.6 10.4V9H23v14h-3.2l-7.6-10.4V23H9z"
		fill="var(--background)"
	/>
	<circle cx="26.2" cy="26.2" r="2.1" fill="url(#{id})" />
</svg>
