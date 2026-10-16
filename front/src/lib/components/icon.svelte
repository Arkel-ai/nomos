<script lang="ts">
	import IconifyIcon from '@iconify/svelte';
	import { boldIcons, type SolarIconName } from '$lib/icons/solar';

	// Every icon in the app renders through here. Solar ships a stroked `linear`
	// cut and a filled `bold` cut of each glyph: linear is the resting state,
	// bold marks the active one (current nav item, selected tab, on state, and
	// icons sitting inside a filled medallion).
	//
	// Importing the generated module registers the bundled icon data with
	// Iconify, so nothing is fetched from api.iconify.design at runtime. Add a
	// name to scripts/icon-map.json and run `npm run icons:sync` to bundle more.
	//
	// The prop list is deliberately narrow rather than a full SVGAttributes
	// spread: Iconify's own props widen the union past what TypeScript will
	// represent. These are the attributes the app actually puts on an icon.
	type Props = {
		/** Solar base name, no style suffix — e.g. `magnifier`. */
		name: SolarIconName;
		/** Render the filled cut. Falls back to linear for icons without one. */
		active?: boolean;
		class?: string;
		style?: string;
		fill?: string;
		'aria-label'?: string;
		'aria-hidden'?: boolean;
	};

	let { name, active = false, class: className = '', ...rest }: Props = $props();

	const icon = $derived(`solar:${name}-${active && boldIcons.has(name) ? 'bold' : 'linear'}`);
</script>

<IconifyIcon {icon} class={className} {...rest} />
