import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
export default {
  preprocess: vitePreprocess(),
  kit: {
    adapter: adapter({ pages: 'build', assets: 'build', fallback: 'index.html', strict: false }),
    paths: { relative: false },
    // Раз в минуту проверять _app/version.json: иммутабельные чанки живут год.
    version: { pollInterval: 60000 }
  }
};
