import { defineConfig } from "vitest/config";
import preact from "@preact/preset-vite";

// The backend embeds the build (backend/web/dist). `npm run dev` serves the UI with
// hot reload and sends /api to a running sisyphus (SISYPHUS_URL, default its usual address).
const backend = process.env.SISYPHUS_URL ?? "http://127.0.0.1:7465";

export default defineConfig({
  plugins: [preact()],
  build: { outDir: "../backend/web/dist", emptyOutDir: true },
  server: { proxy: { "/api": { target: backend, changeOrigin: true } } },
  test: { environment: "jsdom" },
});
