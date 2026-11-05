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
    // The toast library inserts its stylesheet as an inline <style> tag at
    // load, which the console's style-src 'self' policy blocks. Its CSS is
    // imported as a regular stylesheet instead (see main.tsx), so the runtime
    // insertion is disabled here.
    {
      name: 'sonner-static-styles',
      enforce: 'pre',
      transform(code, id) {
        if (!/node_modules[\\/]sonner[\\/]dist[\\/]index\.m?js$/.test(id)) return null;
        const patched = code.replace('function __insertCSS(code) {', 'function __insertCSS(code) {\n  return;');
        if (patched === code) this.error('sonner style injection hook not found; update the sonner-static-styles plugin');
        return { code: patched, map: null };
      },
    },
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
