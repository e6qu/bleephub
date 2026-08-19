import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const MAX_ENTRY_BYTES = 160 * 1024;
const MAX_CHUNK_BYTES = 450 * 1024;

function bundleBudget(): Plugin {
  return {
    name: "bleephub-bundle-budget",
    generateBundle(_options, bundle) {
      for (const item of Object.values(bundle)) {
        if (item.type !== "chunk") continue;
        const limit = item.isEntry ? MAX_ENTRY_BYTES : MAX_CHUNK_BYTES;
        const bytes = Buffer.byteLength(item.code);
        if (bytes > limit) {
          this.error(
            `${item.fileName} is ${bytes} bytes, exceeding the ${item.isEntry ? "entry" : "chunk"} budget of ${limit} bytes`,
          );
        }
      }
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss(), bundleBudget()],
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
          if (id.includes("/highlight.js/")) return "vendor-hljs";
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
