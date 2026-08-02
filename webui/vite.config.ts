import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // Relative asset URLs so the built UI works under a reverse-proxy subpath
  // (the Go side embeds the dist into /stats/).
  base: "./",
  build: {
    outDir: "../server/webui/dist",
    emptyOutDir: true,
  },
  server: {
    proxy: {
      // Dev only: forward /stats/* to a locally running apid.
      "/stats": "http://localhost:19092",
    },
  },
});
