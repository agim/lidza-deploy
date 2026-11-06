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
    // Every /console/… view is a client-side route of console.html (the Go
    // server does the same in production), so deep links work in development.
    { name: 'console-routes', configureServer(server) { server.middlewares.use((req, _res, next) => { if (req.url && /^\/console(\/|\?|$)/.test(req.url)) req.url = '/console.html'; next(); }); } },
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
    // Dialogs and menus lock page scrolling through react-remove-scroll, which
    // inserts a generated <style> element; style-src 'self' blocks it. The
    // lock is styled statically instead (body[data-scroll-locked] in
    // console.css), so the runtime style element is not created.
    {
      name: 'scroll-lock-static-styles',
      enforce: 'pre',
      transform(code, id) {
        if (!/node_modules[\\/]react-style-singleton[\\/]dist[\\/][^\\/]+[\\/]singleton\.js$/.test(id)) return null;
        const patched = code.replace(/function makeStyleTag\(\) \{/, 'function makeStyleTag() {\n    return null;');
        if (patched === code) this.error('react-style-singleton style tag hook not found; update the scroll-lock-static-styles plugin');
        return { code: patched, map: null };
      },
    },
    // The select menu hides its viewport scrollbar through an inline <style>
    // element rendered on every open, which style-src 'self' blocks (one
    // console error per opened select). The same rules live statically in
    // console.css ([data-radix-select-viewport]), so the element is removed.
    {
      name: 'select-viewport-static-styles',
      enforce: 'pre',
      transform(code, id) {
        if (!/node_modules[\\/]@radix-ui[\\/]react-select[\\/]dist[\\/]index\.m?js$/.test(id)) return null;
        const patched = code.replace(/\/\* @__PURE__ \*\/ jsx\(\s*"style",\s*\{\s*dangerouslySetInnerHTML: \{\s*__html: `\[data-radix-select-viewport\][^`]*`\s*\},\s*nonce\s*\}\s*\)/, 'null');
        if (patched === code) this.error('radix select viewport style element not found; update the select-viewport-static-styles plugin');
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
