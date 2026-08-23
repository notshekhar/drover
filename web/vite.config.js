import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const dir = dirname(fileURLToPath(import.meta.url));

export default defineConfig({
  root: dir,
  base: "/dashboard/",
  plugins: [react()],
  build: {
    outDir: resolve(dir, "../internal/web/dist"),
    emptyOutDir: true,
    sourcemap: false,
    // The engine serves the dashboard under `default-src 'self'`, which bans
    // inline script. Vite's module-preload polyfill is inline, so it is off:
    // one entry chunk needs no preloading anyway.
    modulePreload: { polyfill: false },
  },
  server: {
    proxy: {
      "/apis": "http://127.0.0.1:7432",
    },
  },
});
