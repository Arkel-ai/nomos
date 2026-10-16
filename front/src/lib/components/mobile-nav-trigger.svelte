<script lang="ts">
	import Icon from '$lib/components/icon.svelte';
	import { page } from '$app/state';
	import { useSidebar } from '$lib/components/ui/sidebar';
	import { t } from '$lib/i18n';

	// Below md the rail is a sheet with no edge to reach for: this floating
	// button opens it, and a navigation closes it again.
	const sidebar = useSidebar();

	$effect(() => {
		void page.url.pathname;
		sidebar.setOpenMobile(false);
	});
</script>

{#if sidebar.isMobile}
	<button
		type="button"
		onclick={() => sidebar.setOpenMobile(true)}
		aria-label={$t('nav.openMenu')}
		class="bg-card/90 text-foreground fixed top-3 right-3 z-40 flex h-9 w-9 cursor-pointer items-center justify-center rounded-xl border shadow-sm backdrop-blur"
	>
		<Icon name="hamburger-menu" class="h-[18px] w-[18px]" />
	</button>
{/if}
