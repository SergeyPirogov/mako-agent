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
import { browseTrackers, adoptTask } from "../src/browse.ts";

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
    const web = (i: any) => ({ ...i, web_url: `${base}/acme/web/-/issues/${i.iid}` });
    // «Трекер» asks for everything; Mako's search (with its label) gets only #1
    if (url.pathname.endsWith("/issues") && url.searchParams.get("order_by") === "updated_at") return res.end(JSON.stringify(Object.values(issues).map(web)));
    if (url.pathname.endsWith("/issues")) return res.end(JSON.stringify([issues[1]].map(web)));
    if (url.pathname.endsWith("/notes")) return res.end("[]");
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

test("«Трекер» lists every open issue with its assignees and what Mako does with it; «Взяти» takes one Mako left out", async () => {
  const cfg = parseConfig({
    stateDir: path.join(dir, "state2"),
    planFirst: false,
    notifications: { enabled: false, inboxPort: 0 },
    executors: { agent: { type: "command", command: "true" } },
    trackers: [{ type: "gitlab", baseUrl: base, projects: { "acme/web": { repoPath: dir, setup: "none" } } }],
  });
  const tracker = createTracker(cfg.trackers[0], () => "token");
  const o = new Orchestrator(cfg, [tracker], new StateStore(cfg.stateDir));
  try {
    const [view] = await browseTrackers(o);
    assert.equal(view.me, "me");
    const byId = Object.fromEntries(view.items.map((i) => [i.task.id, i]));
    assert.deepEqual(Object.keys(byId), ["acme/web#1", "acme/web#2", "acme/web#3"]);
    assert.deepEqual(byId["acme/web#1"].task.assignees, ["dev", "qa"]);
    assert.deepEqual([byId["acme/web#1"].mako.kind, byId["acme/web#1"].mako.text], ["skipped", "призначена на інших"]);
    assert.deepEqual([byId["acme/web#3"].mako.kind, byId["acme/web#3"].mako.text], ["skipped", "не підходить під фільтр Mako"]);

    const id = await adoptTask(o, `${base}/acme/web/-/issues/3`);
    assert.equal(id, "acme/web#3");
    assert.notEqual(o.state.get(id)?.status, undefined);
    assert.notEqual(o.state.get(id)?.status, "snoozed", "taken, not waiting for an answer");
    await assert.rejects(adoptTask(o, "acme/web#3"), /уже на дошці/);
    const [again] = await browseTrackers(o);
    assert.equal(again.items.find((i) => i.task.id === id)?.mako.kind, "board");
  } finally {
    o.stop();
    await o.drain();
  }
});

