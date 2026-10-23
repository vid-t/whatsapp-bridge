import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { PRODUCT } from "./src/lib/product.ts";
import { THEME_KEY } from "./src/lib/theme-key.ts";

// The bridge the dev and preview servers proxy to. Point it at a test bridge
// or at scripts/mock-bridge.mjs (npm run preview:mock does that for you).
const target = process.env.BRIDGE_URL ?? "http://127.0.0.1:8080";

// Same-origin proxy: the browser talks to Vite, Vite forwards to the bridge.
const proxy = Object.fromEntries(
  ["/api", "/auth", "/mcp", "/oauth", "/.well-known"].map((p) => [p, { target, changeOrigin: false }]),
);

// The exact policy the Go server sends (internal/web/server.go). `vite preview`
// serves the production bundle under it, so CSP problems show up before Go embeds it.
const csp =
  "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; " +
  "base-uri 'none'; form-action 'self'; frame-ancestors 'none'";

// Runs before the first paint, from a hashed file (the CSP allows no inline
// script): it puts the stored or system theme on <html>, so the page never
// flashes the wrong one. src/lib/theme.tsx takes over once React runs.
const THEME_BOOT =
  `(function(){var d;try{var t=localStorage.getItem(${JSON.stringify(THEME_KEY)});` +
  `d=t==="dark"||(t!=="light"&&matchMedia("(prefers-color-scheme: dark)").matches)}catch(e){d=false}` +
  `var r=document.documentElement;r.classList.add(d?"dark":"light")})();`;

const require = createRequire(import.meta.url);
const licence = (pkg: string) => readFileSync(require.resolve(`${pkg}/LICENSE`), "utf8").trim();

/**
 * Adds the theme script to <head>, preloads the Latin text face, and ships the
 * fonts' SIL Open Font License next to them (static/fonts-OFL-<hash>.txt).
 */
function shellExtras(): Plugin {
  return {
    name: "shell-extras",
    buildStart() {
      if (this.meta.watchMode) return;
      this.emitFile({ type: "asset", name: "theme.js", source: THEME_BOOT });
      this.emitFile({
        type: "asset",
        name: "fonts-OFL.txt",
        source:
          "Geist and Geist Mono, by the Geist Project Authors, are bundled under the SIL Open Font License 1.1.\n\n" +
          `--- Geist ---\n${licence("@fontsource-variable/geist")}\n\n--- Geist Mono ---\n${licence("@fontsource-variable/geist-mono")}\n`,
      });
    },
    configureServer(server) {
      server.middlewares.use("/@theme-boot.js", (_req, res) => {
        res.setHeader("Content-Type", "text/javascript");
        res.end(THEME_BOOT);
      });
    },
    transformIndexHtml: {
      order: "post",
      handler(_html, ctx) {
        if (!ctx.bundle) return [{ tag: "script", attrs: { src: "/@theme-boot.js" }, injectTo: "head-prepend" }];
        const files = Object.values(ctx.bundle);
        const named = (n: string) =>
          files.find((f) => f.type === "asset" && ((f.names ?? []).includes(n) || f.fileName.includes(n.replace(/\.[a-z0-9]+$/, "-"))));
        const boot = named("theme.js");
        const face = files.find((f) => f.type === "asset" && /geist-latin-wght-normal-[^/]+\.woff2$/.test(f.fileName));
        if (!boot) throw new Error("shell-extras: theme script missing from the bundle");
        return [
          { tag: "script", attrs: { src: "/" + boot.fileName }, injectTo: "head" },
          ...(face
            ? [{ tag: "link", attrs: { rel: "preload", href: "/" + face.fileName, as: "font", type: "font/woff2", crossorigin: "" }, injectTo: "head" as const }]
            : []),
        ];
      },
    },
  };
}

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    shellExtras(),
    // index.html says %PRODUCT_NAME%, so the name lives only in src/lib/product.ts.
    { name: "product-name", transformIndexHtml: { order: "pre", handler: (html) => html.replaceAll("%PRODUCT_NAME%", PRODUCT) } },
  ],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
      // Radix's scroll lock injects a <style> element, which the CSP refuses.
      // This stand-in locks scrolling with a class from the stylesheet instead.
      "react-remove-scroll": fileURLToPath(new URL("./src/lib/remove-scroll.tsx", import.meta.url)),
    },
  },
  build: {
    outDir: "../internal/web/assets/dist",
    emptyOutDir: true,
    // Flat, hashed files under /static/, which the bridge serves without sign-in.
    assetsDir: "static",
    // Never inline assets as data: URIs; the shell must only reference /static/.
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    target: "es2022",
    sourcemap: false,
  },
  server: { host: "127.0.0.1", port: 5173, strictPort: false, proxy },
  preview: {
    host: "127.0.0.1",
    port: 4174,
    proxy,
    allowedHosts: [".ts.net"],
    headers: {
      "Content-Security-Policy": csp,
      "X-Content-Type-Options": "nosniff",
      "Referrer-Policy": "no-referrer",
      "X-Frame-Options": "DENY",
    },
  },
});
