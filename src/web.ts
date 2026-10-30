import http from "node:http";
import { randomBytes } from "node:crypto";
import { readFile } from "node:fs/promises";
import { existsSync, statSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { run } from "./shell.js";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { createRequire } from "node:module";
import { WebSocketServer } from "ws";
import type { App } from "./app.js";
import { renderInbox, renderArchive, inboxSignature, renderTaskLog } from "./inbox.js";
import { page, esc, setVersion } from "./ui-layout.js";
import { execFileSync } from "node:child_process";
import { sessions, type SessionEvent } from "./sessions.js";
import { collectChanges, readWorktreeFile } from "./changes.js";
import { log } from "./log.js";
import { doctor, findRepos } from "./onboarding.js";

const UI_DIR = fileURLToPath(new URL("../ui/", import.meta.url));
const require = createRequire(import.meta.url);
const ASSETS_DIR = fileURLToPath(new URL("../assets/", import.meta.url));
const STATIC: Record<string, [string, string]> = {
  "favicon.png": [path.join(ASSETS_DIR, "tray.png"), "image/png"],
  "favicon-attention.png": [path.join(ASSETS_DIR, "tray-attention.png"), "image/png"],
  "icon.png": [path.join(ASSETS_DIR, "icon.png"), "image/png"],
  "style.css": [path.join(UI_DIR, "style.css"), "text/css"],
  "settings.js": [path.join(UI_DIR, "settings.js"), "text/javascript"],
  "session.js": [path.join(UI_DIR, "session.js"), "text/javascript"],
  "inbox.js": [path.join(UI_DIR, "inbox.js"), "text/javascript"],
  "dialog.js": [path.join(UI_DIR, "dialog.js"), "text/javascript"],
  "changes.js": [path.join(UI_DIR, "changes.js"), "text/javascript"],
  "welcome.js": [path.join(UI_DIR, "welcome.js"), "text/javascript"],
  "tasklog.js": [path.join(UI_DIR, "tasklog.js"), "text/javascript"],
  "vendor/xterm.js": [require.resolve("@xterm/xterm/lib/xterm.js"), "text/javascript"],
  "vendor/xterm.css": [require.resolve("@xterm/xterm/css/xterm.css"), "text/css"],
  "vendor/addon-fit.js": [require.resolve("@xterm/addon-fit/lib/addon-fit.js"), "text/javascript"],
};

/**
 * Local web UI: inbox and settings. It can rewrite the config (which decides what commands run),
 * so every change needs the per-run token embedded in the pages, and the Host header must be
 * localhost, which blocks other websites and DNS-rebinding attacks.
 */
/** "v0.1.0 · abc1234": the package version and, when running from a git checkout, the commit. */
function makoVersion(): string {
  const root = fileURLToPath(new URL("../", import.meta.url));
  let v = "";
  try {
    v = "v" + JSON.parse(readFileSync(path.join(root, "package.json"), "utf8")).version;
  } catch {
    // packaged without package.json next to dist: no version
  }
  try {
    const sha = execFileSync("git", ["rev-parse", "--short", "HEAD"], { cwd: root, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
    if (sha) v += ` · ${sha}`;
  } catch {
    // not a git checkout (packaged app)
  }
  return v;
}

export function startWebUi(app: App, port: number): void {
  setVersion(makoVersion());
  const token = randomBytes(24).toString("hex");
  const allowedHosts = new Set([`127.0.0.1:${port}`, `localhost:${port}`]);

  const server = http.createServer(async (req, res) => {
    const send = (status: number, type: string, body: string | Buffer, frameable = false) =>
      res
        .writeHead(status, {
          "Content-Type": typeof body === "string" ? `${type}; charset=utf-8` : type,
          "Cache-Control": typeof body === "string" ? "no-store" : "max-age=3600",
          // Only the changes view may be embedded, and only in Mako's own session page.
          "X-Frame-Options": frameable ? "SAMEORIGIN" : "DENY",
        })
        .end(body);
    const json = (status: number, data: unknown) => send(status, "application/json", JSON.stringify(data));

    if (!allowedHosts.has(req.headers.host ?? "")) return send(403, "text/plain", "Forbidden host");
    const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);

    try {
      // Until Mako is configured, the board has nothing to show: start with the setup wizard.
      if (req.method === "GET" && url.pathname === "/" && !app.settings().configured) {
        return res.writeHead(303, { Location: "/welcome" }).end();
      }
      if (req.method === "GET" && url.pathname === "/") return send(200, "text/html", renderInbox(app, token));
      if (req.method === "GET" && url.pathname === "/welcome") {
        const body = `<div id="welcome" class="wz"><p class="muted">Завантаження…</p></div>`;
        return send(200, "text/html", page({ title: "Початок роботи", active: "settings", token, body, script: "/ui/welcome.js" }));
      }
      if (req.method === "GET" && url.pathname === "/log") return send(200, "text/html", renderTaskLog(app, url.searchParams.get("task") ?? "", token));
      if (req.method === "GET" && url.pathname === "/archive") return send(200, "text/html", renderArchive(app, token));
      if (req.method === "GET" && url.pathname === "/settings") {
        const body = `<div id="app"><p class="muted">Завантаження…</p></div>`;
        return send(200, "text/html", page({ title: "Налаштування", active: "settings", token, body, script: "/ui/settings.js" }));
      }
      if (req.method === "GET" && url.pathname.startsWith("/ui/")) {
        const file = STATIC[url.pathname.slice(4)];
        if (!file) return send(404, "text/plain", "Not found");
        return send(200, file[1], file[1].startsWith("image/") ? await readFile(file[0]) : await readFile(file[0], "utf8"));
      }
      if (req.method === "GET" && url.pathname === "/favicon.ico") {
        return send(200, "image/png", await readFile(STATIC["favicon.png"][0]));
      }
      if (req.method === "GET" && url.pathname === "/changes") {
        const id = url.searchParams.get("task") ?? "";
        const embed = url.searchParams.has("embed");
        const ws = app.orchestrator?.workspaceFor(id);
        const head = ws
          ? `<h1>Зміни · <a href="${esc(ws.task.url)}" target="_blank" rel="noreferrer">${esc(id)}</a></h1><p class="muted">${esc(ws.task.title)}</p>`
          : `<h1>Зміни</h1>`;
        const body = `${embed ? "" : head}<div id="changes" class="cx-host"><p class="muted">Завантаження…</p></div>`;
        return send(
          200,
          "text/html",
          page({ title: `Зміни ${id}`, active: "inbox", token, body, script: "/ui/changes.js", data: { task: id }, bare: embed, wide: !embed }),
          true,
        );
      }
      if (req.method === "GET" && url.pathname === "/api/changes") {
        const ws = app.orchestrator?.workspaceFor(url.searchParams.get("task") ?? "");
        if (!ws) return json(404, { error: "Для цієї задачі ще немає worktree" });
        return json(200, await collectChanges(ws.worktree, ws.baseBranch));
      }
      if (req.method === "GET" && url.pathname === "/api/file") {
        const ws = app.orchestrator?.workspaceFor(url.searchParams.get("task") ?? "");
        if (!ws) return json(404, { error: "Для цієї задачі ще немає worktree" });
        return json(200, readWorktreeFile(ws.worktree, url.searchParams.get("path") ?? ""));
      }
      const sessionPage = url.pathname.match(/^\/session\/([\w.-]+)$/);
      if (req.method === "GET" && sessionPage) return send(200, "text/html", renderSession(sessionPage[1], token));

      // Claude Code hooks of a terminal session; each session has its own token.
      const hook = url.pathname.match(/^\/hook\/([\w.-]+)$/);
      if (req.method === "POST" && hook) {
        const session = sessions.get(hook[1]);
        if (!session || url.searchParams.get("token") !== session.hookToken) return json(403, { error: "unknown session" });
        session.onHook(JSON.parse((await readBody(req)) || "{}"));
        return json(200, {});
      }
      if (req.method === "GET" && url.pathname === "/api/inbox-state") return json(200, inboxSignature(app));
      if (req.method === "GET" && url.pathname === "/api/tasks") {
        return json(200, app.orchestrator?.state.all() ?? []);
      }

      // Everything below changes state.
      if (req.method === "POST" && ["/take", "/later", "/retry", "/cancel", "/search-now", "/archive", "/unarchive", "/takeover"].includes(url.pathname)) {
        const form = new URLSearchParams(await readBody(req));
        if (form.get("token") !== token) return send(403, "text/plain", "Bad token: reload the page");
        const id = form.get("id") ?? "";
        const o = app.orchestrator;
        if (o && url.pathname === "/take") {
          o.decide(id, "take", { repo: form.get("repo") || undefined, executor: form.get("executor") || undefined });
        }
        if (o && url.pathname === "/later") o.decide(id, "later");
        if (o && url.pathname === "/retry") {
          o.retry(id, { repo: form.get("repo") || undefined, executor: form.get("executor") || undefined, resume: form.get("resume") === "1" });
        }
        if (o && url.pathname === "/cancel") o.cancel(id);
        if (o && url.pathname === "/takeover") {
          o.takeOver(id);
          // Back to the log: the terminal appears there as soon as the agent has stopped.
          return res.writeHead(303, { Location: `/log?task=${encodeURIComponent(id)}` }).end();
        }
        if (o && url.pathname === "/archive") {
          // One task, or a whole column ("done" or "failed") from its header.
          const column = form.get("column");
          const statuses = column === "done" ? ["done"] : column === "failed" ? ["failed", "cancelled"] : [];
          o.archive(column ? o.state.all().filter((r) => !r.archivedAt && statuses.includes(r.status)).map((r) => r.id) : [id]);
        }
        if (o && url.pathname === "/unarchive") o.unarchive(id);
        const back = form.get("back");
        if (back === "/archive") return res.writeHead(303, { Location: "/archive" }).end();
        if (o && url.pathname === "/search-now") o.searchNow();
        return res.writeHead(303, { Location: "/" }).end();
      }
      if (url.pathname.startsWith("/api/")) {
        if (req.headers["x-mako-token"] !== token) return json(403, { error: "Bad token: reload the page" });
        if (req.method === "GET" && url.pathname === "/api/settings") return json(200, app.settings());
        if (req.method === "GET" && url.pathname === "/api/doctor") return json(200, await doctor());
        if (req.method === "GET" && url.pathname === "/api/find-repo") {
          return json(200, { paths: findRepos(url.searchParams.get("project") ?? "") });
        }
        if (req.method === "GET" && url.pathname === "/api/check-path") {
          return json(200, await checkRepoPath(url.searchParams.get("path") ?? "", url.searchParams.get("base") || "main"));
        }
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
  // Live terminals: output streams to every open page; input and resize come back.
  const wss = new WebSocketServer({ noServer: true, maxPayload: 1_000_000 });
  const allowedOrigins = new Set([...allowedHosts].map((h) => `http://${h}`));
  server.on("upgrade", (req, socket, head) => {
    const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);
    const id = url.pathname.match(/^\/ws\/session\/([\w.-]+)$/)?.[1];
    const session = id ? sessions.get(id) : undefined;
    const trusted =
      allowedHosts.has(req.headers.host ?? "") &&
      allowedOrigins.has(req.headers.origin ?? "") &&
      url.searchParams.get("token") === token;
    if (!trusted || !session) {
      socket.end("HTTP/1.1 403 Forbidden\r\n\r\n");
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => {
      const send = (e: SessionEvent | { t: "init"; d: string; status: string; message?: string; title: string; exited: boolean }) => {
        if (ws.readyState === ws.OPEN) ws.send(JSON.stringify(e));
      };
      send({ t: "init", d: session.output, status: session.status, message: session.message, title: session.task.title, exited: session.status === "exited" });
      const off = session.subscribe(send);
      ws.on("message", (raw) => {
        let msg: any;
        try {
          msg = JSON.parse(String(raw));
        } catch {
          return;
        }
        if (msg.t === "i" && typeof msg.d === "string") session.input(msg.d);
        if (msg.t === "r") session.resize(Number(msg.cols), Number(msg.rows));
        if (msg.t === "finish") session.finish();
        if (msg.t === "cancel") session.cancel();
      });
      ws.on("close", off);
    });
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

function renderSession(id: string, token: string): string {
  const session = sessions.get(id);
  if (!session) {
    const body = `<h1>Сесія</h1><div class="notice">Сесія <code>${esc(id)}</code> не активна. <a href="/">До задач →</a></div>`;
    return page({ title: "Сесія", active: "inbox", token, body });
  }
  const body = `<div class="session-head">
      <div><a href="${esc(session.task.url)}" target="_blank" rel="noreferrer"><strong>${esc(session.task.id)}</strong></a>
        <span class="muted">${esc(session.task.title)}</span></div>
      <div class="session-actions"><span id="status" class="s"></span>
        <button id="toggle-changes" title="Змінені файли і diff">Зміни</button>
        <button id="finish" class="success" title="Закрити агента, закомітити, запушити і відкрити PR">✓ Завершити</button>
        <button id="cancel" class="danger-outline" title="Зупинити без PR">Скасувати</button></div>
    </div>
    <div id="status-msg" class="muted"></div>
    <div class="session-body"><div id="term"></div>
      <iframe id="changes-panel" hidden title="Зміни" data-src="/changes?task=${encodeURIComponent(session.task.id)}&embed=1"></iframe></div>`;
  return page({ title: session.task.id, active: "inbox", token, body, script: "/ui/session.js", wide: true, head: '<link rel="stylesheet" href="/ui/vendor/xterm.css"><script src="/ui/vendor/xterm.js"></script><script src="/ui/vendor/addon-fit.js"></script>', data: { session: id } });
}

/** Live check for the settings page: does the repo path exist, is it git, does origin/<base> exist locally. */
async function checkRepoPath(
  input: string,
  base: string,
): Promise<{ exists: boolean; git: boolean; hasBase?: boolean; defaultBranch?: string; resolved: string }> {
  const resolved = path.resolve(input.startsWith("~/") ? path.join(homedir(), input.slice(2)) : input);
  if (!input || !existsSync(resolved) || !statSync(resolved).isDirectory()) return { exists: false, git: false, resolved };
  const top = await run("git", ["rev-parse", "--show-toplevel"], resolved).catch(() => undefined);
  if (!top || top.code !== 0) return { exists: true, git: false, resolved };
  const ref = await run("git", ["rev-parse", "--verify", "--quiet", `refs/remotes/origin/${base}`], resolved).catch(() => undefined);
  const head = await run("git", ["symbolic-ref", "--short", "refs/remotes/origin/HEAD"], resolved).catch(() => undefined);
  const defaultBranch = head?.code === 0 ? head.stdout.trim().replace(/^origin\//, "") : undefined;
  return { exists: true, git: true, hasBase: ref?.code === 0, defaultBranch, resolved };
}
