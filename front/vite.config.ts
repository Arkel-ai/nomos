import tailwindcss from '@tailwindcss/vite';
import { sveltekit } from '@sveltejs/kit/vite';
import { readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import { defineConfig } from 'vite';

const file = fileURLToPath(new URL('package.json', import.meta.url));
const json = readFileSync(file, 'utf8');
const pkg = JSON.parse(json);

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	define: {
		APP_VERSION: JSON.stringify(pkg.version)
	},
	server: {
		fs: {
			strict: false
		},
		proxy: {
			// All /api/* goes to the Go backend. Better Auth lives at the
			// top-level /auth/* (served by SvelteKit, hooks.server.ts), so it
			// is outside this prefix and needs no exception.
			'/api': {
				target: 'http://localhost:8000',
				changeOrigin: true
			}
		}
	}
});
