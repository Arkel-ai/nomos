export { default as ThinkingOrb } from './thinking-orb.svelte';

export type { ThinkingOrbProps, OrbState, OrbSize, OrbTheme } from './types';

// Power-user surface: the resolved presets + raw frame painters, for driving
// your own canvas.
export { resolvePreset, STATE_TO_MODE, type ModeKey, type Resolved } from './presets';
export { MODE_DRAWS, MODE_FRAMES } from './engine/registry';
export { countDots, scaleCounts, scaleRadii } from './engine/profiles';
export {
	finalizeFrame,
	makeProj,
	radiusScale,
	fibDir,
	hashD,
	vnoise,
	lerp,
	frac,
	angleDelta
} from './engine/core';
export type { ModeFrame } from './engine/types';
export type { ModeOpts } from './engine/profiles';
export type { OrbFrame, Dot, Line } from './engine/core';
