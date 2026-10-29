import http from "node:http";
import { randomBytes } from "node:crypto";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import type { App } from "./app.js";
import { renderInbox } from "./inbox.js";
import { page } from "./ui-layout.js";
import { log } from "./log.js";

const UI_DIR = fileURLToPath(new URL("../ui/", import.meta.url));
const STATIC: Record<string, string> = { "style.css": "text/css", "settings.js": "text/javascript" };

/**
 * Local web UI: inbox and settings. It can rewrite the config (which decides what commands run),
 * so every change needs the per-run token embedded in the pages, and the Host header must be
 * localhost, which blocks other websites and DNS-rebinding attacks.
 */
export function startWebUi(app: App, port: number): void {
  const token = randomBytes(24).toString("hex");
  const allowedHosts = new Set([`127.0.0.1:${port}`, `localhost:${port}`]);

  const server = http.createServer(async (req, res) => {
    const send = (status: number, type: string, body: string) =>
      res.writeHead(status, { "Content-Type": `${type}; charset=utf-8`, "Cache-Control": "no-store", "X-Frame-Options": "DENY" }).end(body);
    const json = (status: number, data: unknown) => send(status, "application/json", JSON.stringify(data));

    if (!allowedHosts.has(req.headers.host ?? "")) return send(403, "text/plain", "Forbidden host");
    const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);

    try {
      if (req.method === "GET" && url.pathname === "/") return send(200, "text/html", renderInbox(app, token));
      if (req.method === "GET" && url.pathname === "/settings") {
        const body = `<div id="app"><p class="muted">Завантаження…</p></div>`;
        return send(200, "text/html", page({ title: "Налаштування", active: "settings", token, body, script: "/ui/settings.js" }));
      }
      if (req.method === "GET" && url.pathname.startsWith("/ui/")) {
        const name = url.pathname.slice(4);
        if (!STATIC[name]) return send(404, "text/plain", "Not found");
        return send(200, STATIC[name], await readFile(path.join(UI_DIR, name), "utf8"));
      }
      if (req.method === "GET" && url.pathname === "/api/tasks") {
        return json(200, app.orchestrator?.state.all() ?? []);
      }

      // Everything below changes state.
      if (req.method === "POST" && ["/take", "/later", "/retry"].includes(url.pathname)) {
        const form = new URLSearchParams(await readBody(req));
        if (form.get("token") !== token) return send(403, "text/plain", "Bad token: reload the page");
        const id = form.get("id") ?? "";
        const o = app.orchestrator;
        if (o && url.pathname === "/take") o.decide(id, "take");
        if (o && url.pathname === "/later") o.decide(id, "later");
        if (o && url.pathname === "/retry") o.retry(id);
        return res.writeHead(303, { Location: "/" }).end();
      }
      if (url.pathname.startsWith("/api/")) {
        if (req.headers["x-mako-token"] !== token) return json(403, { error: "Bad token: reload the page" });
        if (req.method === "GET" && url.pathname === "/api/settings") return json(200, app.settings());
        const body = JSON.parse((await readBody(req)) || "{}");
        if (req.method === "PUT" && url.pathname === "/api/settings") {
          return json(200, { ok: true, ...app.save(body.config, body.secrets ?? {}), problem: app.problem });
        }
        if (req.method === "POST" && url.pathname === "/api/test-tracker") {
          return json(200, await app.testTracker(body.tracker, body.secrets ?? {}));
        }
      }
      send(404, "text/plain", "Not found");
    } catch (err: any) {
      json(400, { error: err.message });
    }
  });
  server.listen(port, "127.0.0.1", () => log("ui", `http://127.0.0.1:${port}`));
  server.on("error", (err) => log("ui", `cannot start on port ${port}: ${err.message}`));
}

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let body = "";
    req.on("data", (d) => {
      body += d;
      if (body.length > 1_000_000) req.destroy(new Error("Request too large"));
    });
    req.on("end", () => resolve(body));
    req.on("error", reject);
  });
}
