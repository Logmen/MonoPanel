import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  server: {
    proxy: {
      // Dev против живой панели: `MP_DEV_TOKEN=<api-токен> vite dev` добавляет
      // Bearer ко всем /api-запросам — можно смотреть интерфейс без входа.
      '/api': {
        target: 'https://localhost:8443',
        changeOrigin: true,
        secure: false,
        headers: process.env.MP_DEV_TOKEN ? { Authorization: 'Bearer ' + process.env.MP_DEV_TOKEN } : undefined
      }
    }
  }
});
