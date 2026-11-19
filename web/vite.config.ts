import { defineConfig } from "vite";
import solid from "@solidjs/vite-plugin";

const target = process.env.CHAINS_WEB_DEV_TARGET ?? "http://127.0.0.1:8080";
export default defineConfig({
  base: "/ui/",
  plugins: [solid()],
  server: {
    host: "127.0.0.1",
    proxy: { "/operator/v1": { target, changeOrigin: false } },
  },
});
