import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { execFileSync } from "node:child_process";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import http, { request as httpRequest } from "node:http";
import { App } from "../src/app.ts";
import { startWebUi } from "../src/web.ts";
import { setNotificationHost } from "../src/notify.ts";

// The local web UI as a browser sees it: host and token checks, pages, and the forms that change things.
let dir: string;
let app: App;
let server: http.Server;
let base: string;
let token = "";

const freePort = () =>
  new Promise<number>((resolve) => {
    const s = net.createServer().listen(0, "127.0.0.1", () => {
      const port = (s.address() as net.AddressInfo).port;
      s.close(() => resolve(port));
    });
  });

before(async () => {
  dir = mkdtempSync(path.join(os.tmpdir(), "mako-web-"));
  process.env.PATH = "/usr/bin:/bin";
  process.env.HOME = dir;
  process.env.GITLAB_TOKEN = "glpat-test";
  setNotificationHost({ ask: async () => "later", inform: () => {} });
  const remote = path.join(dir, "remote.git");
  const repo = path.join(dir, "web");
  execFileSync("git", ["init", "-q", "--bare", "-b", "main", remote]);
  execFileSync("git", ["clone", "-q", remote, repo]);
  for (const args of [["config", "user.email", "t@t"], ["config", "user.name", "T"], ["commit", "-q", "--allow-empty", "-m", "init"], ["push", "-q", "origin", "main"]]) {
    execFileSync("git", args, { cwd: repo });
  }
  const port = await freePort();
  const agent = path.join(dir, "agent.sh");
  writeFileSync(agent, "#!/bin/sh\necho ok > ok.txt\n", { mode: 0o755 });
  writeFileSync(
    path.join(dir, "mako.config.json"),
    JSON.stringify({
      stateDir: path.join(dir, "state"),
      pollIntervalSec: 3600,
      planFirst: false,
      notifications: { enabled: false, inboxPort: port },
      executors: { agent: { type: "command", command: agent } },
      defaultExecutor: "agent",
      // an unreachable GitLab: searches fail quietly, the UI does not care
      trackers: [{ type: "gitlab", baseUrl: "http://127.0.0.1:9", projects: { "acme/web": { repoPath: repo, pullRequest: "none", setup: "none" } } }],
    }),
  );
  app = new App(path.join(dir, "mako.config.json"), path.join(dir, ".env"));
  app.apply();
  server = startWebUi(app, port);
  base = `http://127.0.0.1:${port}`;
  await new Promise((r) => setTimeout(r, 200));
  token = (await (await fetch(`${base}/`)).text()).match(/name="mako-token" content="([0-9a-f]+)"/)?.[1] ?? "";
});

after(async () => {
  app.orchestrator?.stop();
  await app.orchestrator?.drain();
  server.close();
  rmSync(dir, { recursive: true, force: true });
});

const form = (route: string, fields: Record<string, string>) =>
  fetch(`${base}${route}`, { method: "POST", body: new URLSearchParams(fields), redirect: "manual" });

test("pages render and carry a per-run token", async () => {
  assert.match(token, /^[0-9a-f]{48}$/);
  for (const [route, text] of [["/", "Задачі"], ["/settings", "Налаштування"], ["/stats", "Статистика"], ["/memory", "Памʼять репозиторіїв"], ["/archive", "Архів"]]) {
    const res = await fetch(base + route);
    assert.equal(res.status, 200, route);
    assert.match(await res.text(), new RegExp(text), route);
  }
});

test("other hosts and requests without the token are refused", async () => {
  const foreign = await new Promise<number>((resolve) => {
    // fetch() will not send a different Host header, so ask with http directly
    const req = httpRequest(`${base}/`, { headers: { Host: "evil.example:80" } }, (res: http.IncomingMessage) => resolve(res.statusCode!));
    req.end();
  });
  assert.equal(foreign, 403);
  assert.equal((await fetch(`${base}/api/settings`)).status, 403);
  assert.equal((await fetch(`${base}/api/settings`, { headers: { "X-Mako-Token": token } })).status, 200);
  assert.equal((await form("/create-task", { title: "x", repo: "web" })).status, 403);
});

test("«+ Задача» from the board runs the task; its title is escaped on the board", async () => {
  const res = await form("/create-task", { token, title: "<script>alert(1)</script> write ok.txt", repo: "web", planShown: "1" });
  assert.equal(res.status, 303);
  await app.orchestrator!.drain();
  const rec = app.orchestrator!.state.get("mako#1")!;
  assert.equal(rec.status, "done", rec.error);
  const board = await (await fetch(`${base}/`)).text();
  assert.doesNotMatch(board, /<script>alert\(1\)<\/script>/);
  assert.match(board, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
});

test("repo notes are saved from their page", async () => {
  const res = await form("/memory", { token, repo: "web", text: "- Tests: `pnpm test`" });
  assert.equal(res.status, 303);
  assert.match(await (await fetch(`${base}/memory`)).text(), /pnpm test/);
});
