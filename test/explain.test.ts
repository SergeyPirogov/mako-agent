import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { mkdtempSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { parseConfig } from "../src/config.ts";
import { createTracker } from "../src/trackers/index.ts";
import { Orchestrator } from "../src/orchestrator.ts";
import { StateStore } from "../src/state.ts";

// «Чому Mako не бачить задачу?» against a GitLab that, like some instances, leaves an issue out of its search.
const issues: Record<number, any> = {
  1: { iid: 1, id: 101, state: "opened", title: "Dev and QA on it", labels: ["claude-agent"], assignees: [{ username: "dev" }, { username: "qa" }] },
  2: { iid: 2, id: 102, state: "opened", title: "Mine, but hidden from search", labels: ["claude-agent"], assignees: [{ username: "dev" }, { username: "me" }] },
  3: { iid: 3, id: 103, state: "opened", title: "No label", labels: [], assignees: [] },
};
let server: http.Server;
let dir = "";
let base = "";

before(async () => {
  server = http.createServer((req, res) => {
    res.setHeader("content-type", "application/json");
    const url = new URL(req.url!, "http://x");
    if (url.pathname === "/api/v4/user") return res.end(JSON.stringify({ id: 5, username: "me" }));
    const one = url.pathname.match(/\/issues\/(\d+)$/);
    if (one) return issues[+one[1]] ? res.end(JSON.stringify({ ...issues[+one[1]], web_url: `${base}/acme/web/-/issues/${one[1]}` })) : res.writeHead(404).end("{}");
    if (url.pathname.endsWith("/issues")) return res.end(JSON.stringify([issues[1]].map((i) => ({ ...i, web_url: "" }))));
    res.writeHead(404).end("{}");
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${(server.address() as any).port}`;
  dir = mkdtempSync(path.join(os.tmpdir(), "mako-explain-"));
});
after(() => {
  server.close();
  rmSync(dir, { recursive: true, force: true });
});

test("«Чому Mako не бачить задачу?» names the filter that leaves the issue out", async () => {
  const cfg = parseConfig({
    stateDir: path.join(dir, "state"),
    notifications: { enabled: false, inboxPort: 0 },
    executors: { agent: { type: "command", command: "true" } },
    trackers: [{ type: "gitlab", baseUrl: base, projects: { "acme/web": { repoPath: dir } } }],
  });
  const tracker = createTracker(cfg.trackers[0], () => "token");
  const o = new Orchestrator(cfg, [tracker], new StateStore(cfg.stateDir));
  try {
    const colleagues = await o.explainTask(`${base}/acme/web/-/issues/1`);
    assert.equal(colleagues.id, "acme/web#1");
    assert.match(colleagues.reasons.join("\n"), /призначена на інших \(dev, qa\), а ви — me/);

    const hidden = await o.explainTask("acme/web#2");
    assert.match(hidden.reasons.join("\n"), /пошук GitLab її не повертає/);
    assert.doesNotMatch(hidden.reasons.join("\n"), /призначена на інших/, "I am one of its assignees");

    const unlabeled = await o.explainTask("3");
    assert.match(unlabeled.reasons.join("\n"), /немає мітки «claude-agent»/);

    const unknown = await o.explainTask("something else");
    assert.equal(unknown.ok, false);
    assert.match(unknown.reasons[0], /Не впізнав задачу/);
  } finally {
    o.stop();
  }
});
