<script lang="ts">
	import { cn } from '$lib/utils';
	import { onMount, type Snippet } from 'svelte';

	interface Props {
		gradientBackgroundStart?: string;
		gradientBackgroundEnd?: string;
		firstColor?: string;
		secondColor?: string;
		thirdColor?: string;
		fourthColor?: string;
		fifthColor?: string;
		pointerColor?: string;
		size?: string;
		blur?: string;
		blendingValue?: string;
		class?: string;
		interactive?: boolean;
		containerClass?: string;
		children?: Snippet;
	}

	let {
		gradientBackgroundStart = 'rgb(108, 0, 162)',
		gradientBackgroundEnd = 'rgb(0, 17, 82)',
		firstColor = '18, 113, 255',
		secondColor = '221, 74, 255',
		thirdColor = '100, 220, 255',
		fourthColor = '200, 50, 50',
		fifthColor = '180, 180, 50',
		pointerColor = '140, 100, 255',
		size = '80%',
		blur = '40px',
		blendingValue = 'hard-light',
		class: className,
		interactive = true,
		containerClass,
		children,
	}: Props = $props();

	let containerEl: HTMLDivElement | undefined = $state();
	let interactiveRef: HTMLDivElement | undefined = $state();

	let curX = 0;
	let curY = 0;
	let tgX = 0;
	let tgY = 0;
	let animFrame = 0;

	function animate() {
		if (!interactiveRef) return;
		curX += (tgX - curX) / 20;
		curY += (tgY - curY) / 20;
		interactiveRef.style.transform = `translate(${Math.round(curX)}px, ${Math.round(curY)}px)`;
		animFrame = requestAnimationFrame(animate);
	}

	onMount(() => {
		if (interactive) {
			animFrame = requestAnimationFrame(animate);
		}
		return () => cancelAnimationFrame(animFrame);
	});

	function handleMouseMove(event: MouseEvent) {
		if (!interactiveRef) return;
		const rect = interactiveRef.getBoundingClientRect();
		tgX = event.clientX - rect.left;
		tgY = event.clientY - rect.top;
	}
</script>

<div
	bind:this={containerEl}
	class={cn(
		'gradient-animation relative left-0 top-0 h-96 w-full overflow-hidden',
		containerClass
	)}
	style:--gradient-background-start={gradientBackgroundStart}
	style:--gradient-background-end={gradientBackgroundEnd}
	style:--first-color={firstColor}
	style:--second-color={secondColor}
	style:--third-color={thirdColor}
	style:--fourth-color={fourthColor}
	style:--fifth-color={fifthColor}
	style:--pointer-color={pointerColor}
	style:--size={size}
	style:--blending-value={blendingValue}
>
	<svg class="hidden">
		<defs>
			<filter id="blurMe">
				<feGaussianBlur in="SourceGraphic" stdDeviation="10" result="blur" />
				<feColorMatrix
					in="blur"
					mode="matrix"
					values="1 0 0 0 0  0 1 0 0 0  0 0 1 0 0  0 0 0 18 -8"
					result="goo"
				/>
				<feBlend in="SourceGraphic" in2="goo" />
			</filter>
		</defs>
	</svg>

	<div class={cn('relative z-10', className)}>
		{#if children}
			{@render children()}
		{/if}
	</div>

	<div class="gradients-container absolute inset-0 h-full w-full" style="filter: url(#blurMe) blur({blur});">
		<!-- First -->
		<div
			class="ga-blob ga-first absolute opacity-100"
			style="background: radial-gradient(circle at center, rgba(var(--first-color), 0.8) 0, rgba(var(--first-color), 0) 50%) no-repeat;"
		></div>
		<!-- Second -->
		<div
			class="ga-blob ga-second absolute opacity-100"
			style="background: radial-gradient(circle at center, rgba(var(--second-color), 0.8) 0, rgba(var(--second-color), 0) 50%) no-repeat; transform-origin: calc(50% - 400px);"
		></div>
		<!-- Third -->
		<div
			class="ga-blob ga-third absolute opacity-100"
			style="background: radial-gradient(circle at center, rgba(var(--third-color), 0.8) 0, rgba(var(--third-color), 0) 50%) no-repeat; transform-origin: calc(50% + 400px);"
		></div>
		<!-- Fourth -->
		<div
			class="ga-blob ga-fourth absolute opacity-70"
			style="background: radial-gradient(circle at center, rgba(var(--fourth-color), 0.8) 0, rgba(var(--fourth-color), 0) 50%) no-repeat; transform-origin: calc(50% - 200px);"
		></div>
		<!-- Fifth -->
		<div
			class="ga-blob ga-fifth absolute opacity-100"
			style="background: radial-gradient(circle at center, rgba(var(--fifth-color), 0.8) 0, rgba(var(--fifth-color), 0) 50%) no-repeat; transform-origin: calc(50% - 800px) calc(50% + 800px);"
		></div>

		{#if interactive}
			<div
				bind:this={interactiveRef}
				onmousemove={handleMouseMove}
				class="absolute -left-1/2 -top-1/2 h-full w-full opacity-70"
				style="background: radial-gradient(circle at center, rgba(var(--pointer-color), 0.8) 0, rgba(var(--pointer-color), 0) 50%) no-repeat; mix-blend-mode: var(--blending-value);"
			></div>
		{/if}
	</div>
</div>

<style>
	.gradient-animation {
		background: linear-gradient(40deg, var(--gradient-background-start), var(--gradient-background-end));
	}

	.ga-blob {
		left: calc(50% - var(--size) / 2);
		top: calc(50% - var(--size) / 2);
		height: var(--size);
		width: var(--size);
		mix-blend-mode: var(--blending-value);
		transform-origin: center center;
	}

	.ga-first {
		animation: ga-moveVertical 25s ease infinite;
	}

	.ga-second {
		animation: ga-moveInCircle 15s reverse infinite;
	}

	.ga-third {
		animation: ga-moveInCircle 25s linear infinite;
	}

	.ga-fourth {
		animation: ga-moveHorizontal 40s ease infinite;
	}

	.ga-fifth {
		animation: ga-moveInCircle 15s ease infinite;
	}

	@keyframes ga-moveHorizontal {
		0% {
			transform: translateX(-50%) translateY(-10%);
		}
		50% {
			transform: translateX(50%) translateY(10%);
		}
		100% {
			transform: translateX(-50%) translateY(-10%);
		}
	}

	@keyframes ga-moveInCircle {
		0% {
			transform: rotate(0deg);
		}
		50% {
			transform: rotate(180deg);
		}
		100% {
			transform: rotate(360deg);
		}
	}

	@keyframes ga-moveVertical {
		0% {
			transform: translateY(-50%);
		}
		50% {
			transform: translateY(50%);
		}
		100% {
			transform: translateY(-50%);
		}
	}
</style>
