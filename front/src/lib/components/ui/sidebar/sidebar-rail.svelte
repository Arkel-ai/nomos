<script lang="ts">
	import { cn, type WithElementRef } from "$lib/utils.js";
	import type { HTMLAttributes } from "svelte/elements";
	import { useSidebar } from "./context.svelte.js";

	let {
		ref = $bindable(null),
		class: className,
		children,
		...restProps
	}: WithElementRef<HTMLAttributes<HTMLDivElement>, HTMLDivElement> = $props();

	const sidebar = useSidebar();

	function handleMouseDown(e: MouseEvent) {
		if (sidebar.state === "collapsed") {
			sidebar.toggle();
			return;
		}

		e.preventDefault();

		const sidebarEl = (e.currentTarget as HTMLElement).closest("[data-slot='sidebar']");
		const side = sidebarEl?.getAttribute("data-side") ?? "left";

		const startX = e.clientX;
		const startWidth = sidebar.width;
		let hasDragged = false;

		function handleMouseMove(e: MouseEvent) {
			if (!hasDragged && Math.abs(e.clientX - startX) < 4) return;

			if (!hasDragged) {
				hasDragged = true;
				sidebar.isResizing = true;
				document.body.style.userSelect = "none";
				document.body.style.cursor = "col-resize";
			}

			const delta = side === "left" ? e.clientX - startX : startX - e.clientX;
			sidebar.setWidth(startWidth + delta);
		}

		function handleMouseUp() {
			if (hasDragged) {
				sidebar.isResizing = false;
				sidebar.persistWidth();
				document.body.style.userSelect = "";
				document.body.style.cursor = "";
			}

			document.removeEventListener("mousemove", handleMouseMove);
			document.removeEventListener("mouseup", handleMouseUp);
		}

		document.addEventListener("mousemove", handleMouseMove);
		document.addEventListener("mouseup", handleMouseUp);
	}

	function handleDoubleClick(e: MouseEvent) {
		e.preventDefault();
		if (sidebar.state !== "collapsed") {
			sidebar.resetWidth();
		}
	}
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
	bind:this={ref}
	data-sidebar="rail"
	data-slot="sidebar-rail"
	tabindex={-1}
	onmousedown={handleMouseDown}
	ondblclick={handleDoubleClick}
	class={cn(
		"hover:after:bg-sidebar-border absolute inset-y-0 z-20 hidden w-4 -translate-x-1/2 transition-all ease-linear after:absolute after:inset-y-0 after:left-[calc(1/2*100%-1px)] after:w-[2px] group-data-[side=left]:-right-4 group-data-[side=right]:left-0 sm:flex",
		sidebar.state === "collapsed"
			? "in-data-[side=left]:cursor-e-resize in-data-[side=right]:cursor-w-resize"
			: "cursor-col-resize",
		"hover:group-data-[collapsible=offcanvas]:bg-sidebar group-data-[collapsible=offcanvas]:translate-x-0 group-data-[collapsible=offcanvas]:after:left-full",
		"[[data-side=left][data-collapsible=offcanvas]_&]:-right-2",
		"[[data-side=right][data-collapsible=offcanvas]_&]:-left-2",
		className
	)}
	{...restProps}
>
	{@render children?.()}
</div>
