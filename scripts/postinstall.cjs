// node-pty's macOS prebuilds ship spawn-helper without the execute bit, which makes every
// terminal session fail with "posix_spawnp failed". Restore it after install.
const fs = require("fs");
const path = require("path");
if (process.platform === "win32") process.exit(0);
let root;
try {
  root = path.dirname(require.resolve("node-pty/package.json"));
} catch {
  process.exit(0);
}
for (const dir of ["build/Release", ...fs.readdirSync(path.join(root, "prebuilds")).map((d) => `prebuilds/${d}`)]) {
  const helper = path.join(root, dir, "spawn-helper");
  if (fs.existsSync(helper)) fs.chmodSync(helper, 0o755);
}
