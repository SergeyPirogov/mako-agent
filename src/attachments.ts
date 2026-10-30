import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { Task, Tracker } from "./types.js";

export const MAX_ATTACHMENTS = 10;
export const MAX_ATTACHMENT_BYTES = 15_000_000;

/**
 * Downloads a task's files (screenshots, logs, specs) into `dir`, with the tracker's credentials, so the agent
 * can open them; returns the prompt section that lists them (empty without files). A file that does not come
 * is named in the log, and the agent works without it.
 */
export async function fetchAttachments(
  task: Task,
  tracker: Tracker,
  dir: string,
  shownDir: string,
  log: (line: string) => void,
): Promise<string> {
  const files = task.attachments ?? [];
  if (!files.length || !tracker.downloadAttachment) return "";
  mkdirSync(dir, { recursive: true });
  const used = new Set<string>();
  const lines: string[] = [];
  const failed: string[] = [];
  for (const a of files.slice(0, MAX_ATTACHMENTS)) {
    const name = uniqueName(safeName(a.name), used);
    const file = path.join(dir, name);
    try {
      if (!existsSync(file)) writeFileSync(file, await tracker.downloadAttachment(a, MAX_ATTACHMENT_BYTES));
      lines.push(`- \`${path.join(shownDir, name)}\` (${a.where})`);
    } catch (err: any) {
      failed.push(`${a.name}: ${err.message}`);
    }
  }
  if (files.length > MAX_ATTACHMENTS) failed.push(`ще ${files.length - MAX_ATTACHMENTS} файлів понад ліміт ${MAX_ATTACHMENTS}`);
  log(`attachments: ${lines.length} of ${files.length} downloaded${failed.length ? `; not downloaded: ${failed.join("; ")}` : ""}`);
  if (!lines.length) return "";
  return (
    "\n\n## Attachments\n\nFiles of the issue, downloaded for you (open images with the Read tool to see them; " +
    "they are not part of the repository, do not commit them):\n" +
    lines.join("\n")
  );
}

function safeName(name: string): string {
  const clean = name.replace(/[/\\\\:*?"<>|\u0000-\u001f]+/g, "_").replace(/^\.+/, "").trim();
  return clean.slice(-120) || "file";
}

function uniqueName(name: string, used: Set<string>): string {
  let candidate = name;
  for (let i = 2; used.has(candidate.toLowerCase()); i++) {
    const ext = path.extname(name);
    candidate = `${path.basename(name, ext)}-${i}${ext}`;
  }
  used.add(candidate.toLowerCase());
  return candidate;
}
