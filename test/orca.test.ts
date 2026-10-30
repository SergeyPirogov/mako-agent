import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { OrcaExecutor } from "../src/executors/orca.ts";
import type { ExecContext } from "../src/executors/types.ts";

// A fake Orca CLI: "worktree list" answers only while the app "runs" (a marker file exists).
function fakeOrca(dir: string): string {
  const cli = path.join(dir, "orca");
  writeFileSync(
    cli,
    `#!/bin/sh
if [ "$2" = "list" ]; then [ -f "${dir}/running" ] && echo '[]' && exit 0; echo '{"ok":false,"error":{"code":"app_not_running","message":"Orca is not running"}}'; exit 1; fi
if [ "$2" = "create" ]; then [ -f "${dir}/running" ] || { echo '{"ok":false,"error":{"code":"app_not_running","message":"Orca is not running"}}'; exit 1; }
  echo '{"worktree":{"path":"${dir}/ws","branch":"refs/heads/feat/5-x-2"}}'; exit 0; fi
exit 1
`,
    { mode: 0o755 },
  );
  return cli;
}

const ctx = (dir: string): ExecContext =>
  ({
    task: { id: "acme/web#5", slug: "web-5", title: "x", url: "https://git.example/i/5", trackerId: "gitlab", project: "acme/web", description: "", labels: [], components: [], comments: [] },
    repo: { name: "web", repoPath: dir, baseBranch: "main", worktreesDir: dir, pullRequest: "none", setup: "none", branchPattern: "" },
    branch: "feat/5-x",
    logPath: path.join(dir, "log.jsonl"),
    promptFile: path.join(dir, "p.md"),
    timeoutMin: 1,
  }) as unknown as ExecContext;

test("a closed Orca gives a clear message instead of a failed hand-off", { skip: process.platform === "darwin" && "opens the real app on macOS" }, async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "mako-orca-"));
  const orca = new OrcaExecutor("orca", { type: "orca", command: fakeOrca(dir) });
  await assert.rejects(orca.run(ctx(dir), "do it"), /Orca не запущена/);
});

test("with Orca running the task is handed off on Orca's branch", async () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), "mako-orca-"));
  writeFileSync(path.join(dir, "running"), "");
  const orca = new OrcaExecutor("orca", { type: "orca", command: fakeOrca(dir) });
  const out = await orca.run(ctx(dir), "do it");
  assert.equal(out.kind, "handed-off");
  assert.equal(out.kind === "handed-off" && out.branch, "feat/5-x-2");
});
