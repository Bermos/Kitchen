import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";
import ui from "@nuxt/ui/vite";

// The dev server proxies /api to a locally running operator (`make run` serves
// it on :8082), so the SPA can be developed against a real cluster. Point
// VITE_API_PROXY somewhere else to develop against a remote installation.
export default defineConfig(({ mode }) => ({
  plugins: [
    vue(),
    ui({
      ui: {
        colors: {
          primary: "blue",
          neutral: "neutral",
        },
      },
      // Bundle every icon the source uses. The default is to fetch icon data
      // from the Iconify API at runtime, and a dashboard for self-hosted
      // clusters must not depend on the outside internet to render.
      icon: {
        clientBundle: { scan: true },
      },
    }),
  ],
  server: {
    proxy: {
      "/api": process.env.VITE_API_PROXY ?? "http://localhost:8082",
    },
  },
  build: {
    // The operator embeds the built UI; keep the output deterministic.
    outDir: "dist",
    emptyOutDir: true,
  },
}));
