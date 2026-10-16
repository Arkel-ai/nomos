import adapter from '@sveltejs/adapter-node';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

// adapter-node (not adapter-static): the app needs a Node runtime so
// Better Auth can run its `/api/auth/*` handler and talk to Postgres.
// Pages still render client-side (ssr=false in +layout.ts) — the server
// exists for auth, not for SSR.
const config = {
	preprocess: vitePreprocess({ script: true }),
	kit: {
		adapter: adapter()
	}
};

export default config;
