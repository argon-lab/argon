import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In dev, proxy the API to a local engine (`argon console --no-browser`
// or the standalone api binary). In production the SPA is served by the
// engine itself, so everything is same-origin.
const apiOrigin = process.env.ARGON_API_ORIGIN ?? "http://127.0.0.1:1818";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": apiOrigin,
      "/health": apiOrigin,
    },
  },
});
