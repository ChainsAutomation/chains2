import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";
import solid from "@solidjs/vite-plugin";

const target = process.env.CHAINS_WEB_DEV_TARGET ?? "http://127.0.0.1:8080";
export default defineConfig({
  appType: "mpa",
  plugins: [solid(), {
    name: "chains-web-entrypoints",
    configureServer(server) {
      server.middlewares.use((request, _, next) => {
        if (request.method === "GET" && request.url) {
          const url = new URL(request.url, "http://localhost");
          if (/^\/ui(?:\/(?:state|devices|watch|send|rules|trace)?)?$/.test(url.pathname)) request.url = "/ui/index.html" + url.search;
          else if (/^\/(?:devices|admin|dashboards\/[^/]+)?$/.test(url.pathname)) request.url = "/index.html" + url.search;
        }
        next();
      });
    },
  }],
  build: { rolldownOptions: { input: [
    fileURLToPath(new URL("./index.html", import.meta.url)),
    fileURLToPath(new URL("./ui/index.html", import.meta.url)),
  ] } },
  server: {
    host: "127.0.0.1",
    proxy: { "/operator/v1": { target, changeOrigin: false }, "/household/v1": { target, changeOrigin: false } },
  },
});
