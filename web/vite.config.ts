import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "/ui/",
  build: {
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (!id.includes("/node_modules/")) return undefined;
          if (id.includes("/@bleephub/ui-core/")) return undefined;
          if (
            id.includes("/react/") ||
            id.includes("/react-dom/") ||
            id.includes("/react-router/") ||
            id.includes("/scheduler/")
          ) {
            return "vendor-react";
          }
          if (id.includes("/@tanstack/")) return "vendor-tanstack";
          if (id.includes("/libsodium-wrappers/")) return "vendor-crypto";
          if (id.includes("/yaml/")) return "vendor-yaml";
          return "vendor-misc";
        },
      },
    },
  },
  server: {
    proxy: {
      "/internal": "http://localhost:5555",
      "/health": "http://localhost:5555",
      "/api": "http://localhost:5555",
      "/login": "http://localhost:5555",
      "/auth": "http://localhost:5555",
      "/settings": "http://localhost:5555",
      "/classroom-data": "http://localhost:5555",
      "/a": "http://localhost:5555",
    },
  },
});
