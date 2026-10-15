import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { resolve } from 'node:path';
import { rmSync } from 'node:fs';

const consoleAssets = resolve(__dirname, '../web/static/assets/console');

export default defineConfig({
  root: resolve(__dirname),
  resolve: { alias: { '@': resolve(__dirname, 'src') } },
  plugins: [
    { name: 'clean-console-assets', buildStart() { rmSync(consoleAssets, { recursive: true, force: true }); } },
    react(),
    tailwindcss(),
  ],
  build: {
    outDir: resolve(__dirname, '../web/static'),
    assetsDir: 'assets/console',
    emptyOutDir: false,
    sourcemap: false,
    rollupOptions: { input: resolve(__dirname, 'console.html') },
  },
  server: { proxy: { '/api': 'http://127.0.0.1:3000' } },
});
