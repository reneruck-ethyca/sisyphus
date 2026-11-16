import { defineConfig } from "vitest/config";
import preact from "@preact/preset-vite";

// The backend embeds the build (backend/web/dist). `npm run dev` serves the UI with
// hot reload and sends /api to a running sisyphus (SISYPHUS_URL, default its usual address).
const backend = process.env.SISYPHUS_URL ?? "http://127.0.0.1:7465";

export default defineConfig({
  plugins: [preact()],
  // ponytail: the chunks over 500 kB are mermaid's lazy ones (elk ~1.5 MB), served from localhost
  build: { outDir: "../backend/web/dist", emptyOutDir: true, chunkSizeWarningLimit: 1600 },
  // 127.0.0.1, not localhost: the session cookie a dashboard sign-in set for 127.0.0.1 then reaches the dev server too.
  server: { host: "127.0.0.1", proxy: { "/api": { target: backend, changeOrigin: true } } },
  test: { environment: "jsdom" },
});
