// Checks the production bundle against the bridge's CSP and size budget:
// no inline <script> or <style>, no style attributes, every asset under
// /static/, no eval-like calls, no injected <style> elements, the theme
// script in <head> as a plain (blocking) script so the page never flashes
// the wrong theme, bundled fonts with their licence, and a filled-in title.
import { readFile, readdir } from "node:fs/promises";
import { gzipSync } from "node:zlib";
import { fileURLToPath } from "node:url";
import path from "node:path";

const dist = fileURLToPath(new URL("../../internal/web/assets/dist/", import.meta.url));
const fail = [];
const html = await readFile(path.join(dist, "index.html"), "utf8");

for (const m of html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)) {
  if (!/\bsrc=/.test(m[1]) || m[2].trim()) fail.push("inline <script> in index.html");
}
if (/<style\b/i.test(html)) fail.push("<style> in index.html");
if (/\sstyle=/i.test(html)) fail.push("style= attribute in index.html");
if (html.includes("<script>")) fail.push('literal "<script>" in index.html (server_test rejects it)');
if (html.includes("%PRODUCT_NAME%")) fail.push("index.html still says %PRODUCT_NAME%");
if (!/<title>[^<]+<\/title>/.test(html)) fail.push("index.html has no title");
for (const m of html.matchAll(/(?:src|href)="([^"]+)"/g)) {
  if (!m[1].startsWith("/static/")) fail.push(`asset outside /static/: ${m[1]}`);
}
const head = html.slice(0, html.indexOf("</head>"));
if (!/<script src="\/static\/theme-[A-Za-z0-9_-]{8}\.js"><\/script>/.test(head)) fail.push("no blocking theme script in <head>");

const top = await readdir(dist);
for (const f of top) if (f !== "index.html" && f !== "static") fail.push(`unexpected file in dist: ${f}`);

const files = await readdir(path.join(dist, "static"));
let js = 0;
let css = 0;
let fonts = 0;
for (const f of files) {
  const buf = await readFile(path.join(dist, "static", f));
  if (!/\.(js|css|svg|woff2|txt)$/.test(f)) fail.push(`unexpected asset ${f} (only JS, CSS, fonts, their licence and the SVG icon are expected)`);
  if (f.endsWith(".txt") && !/^fonts-OFL-/.test(f)) fail.push(`unexpected text file ${f}`);
  if (f.endsWith(".woff2")) fonts += buf.length;
  if (f.endsWith(".js")) {
    js += gzipSync(buf).length;
    const s = buf.toString();
    if (/\beval\(|new Function\(/.test(s)) fail.push(`${f}: eval or new Function`);
    if (/createElement\(["']style["']\)/.test(s)) fail.push(`${f}: creates <style> elements`);
    if (/setAttribute\(["']style["']/.test(s)) fail.push(`${f}: sets style attributes`);
  } else if (f.endsWith(".css")) {
    css += gzipSync(buf).length;
    const text = buf.toString();
    if (/@import|url\(\s*["']?(https?:|\/\/|data:)/i.test(text)) fail.push(`${f}: loads something from outside or inlines data`);
    for (const m of text.matchAll(/url\(\s*["']?([^"')]+)/g)) if (!m[1].startsWith("/static/")) fail.push(`${f}: url(${m[1]}) outside /static/`);
  }
}
if (fonts > 0 && !files.some((f) => /^fonts-OFL-.*\.txt$/.test(f))) fail.push("fonts ship without their licence (fonts-OFL-*.txt)");

const kb = (n) => (n / 1024).toFixed(1) + " KB";
console.log(`dist: ${files.length} files in static/; JS ${kb(js)} gzip, CSS ${kb(css)} gzip, fonts ${kb(fonts)} (woff2, loaded per script)`);
if (js > 180 * 1024) fail.push(`JS over budget: ${kb(js)} gzip > 180 KB`);
if (css > 24 * 1024) fail.push(`CSS over budget: ${kb(css)} gzip > 24 KB`);
if (fail.length) {
  console.error("check-dist failed:\n  " + fail.join("\n  "));
  process.exit(1);
}
console.log("check-dist: ok (CSP-safe shell, flat /static/ assets)");
