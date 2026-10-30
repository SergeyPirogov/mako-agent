import type { Attachment, Task } from "../types.js";

/**
 * Files linked from Markdown (`![alt](url)`, `[name](url)`, `<img src>`) in an issue's description and comments
 * that `pick` recognises as the tracker's own uploads, each once, with where the issue mentions it.
 */
export function linkedFiles(task: Pick<Task, "description" | "comments">, pick: (url: string) => string | undefined): Attachment[] {
  const found = new Map<string, Attachment>();
  const scan = (text: string, where: string) => {
    const links = [
      ...[...text.matchAll(/!?\[([^\]]*)\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)/g)].map((m) => ({ text: m[1], url: m[2] })),
      ...[...text.matchAll(/<img\b[^>]*\bsrc=["']([^"']+)["'][^>]*>/gi)].map((m) => ({ text: "", url: m[1] })),
    ];
    for (const link of links) {
      const url = pick(link.url);
      if (!url || found.has(url)) continue;
      const file = decodeURIComponent(url.split(/[?#]/)[0].split("/").pop() || "file");
      found.set(url, { name: file, url, where: link.text && link.text !== file ? `${where}: «${link.text}»` : where });
    }
  };
  scan(task.description ?? "", "опис");
  for (const c of task.comments ?? []) scan(c.body ?? "", `коментар ${c.author}`);
  return [...found.values()];
}
