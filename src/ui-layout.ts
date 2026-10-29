export function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}

/** Shared page shell for the inbox and settings pages. `token` protects every state-changing request. */
export function page(opts: { title: string; active: "inbox" | "settings"; token: string; body: string; script?: string; refresh?: number }): string {
  const tab = (href: string, id: string, label: string) =>
    `<a href="${href}" class="${opts.active === id ? "on" : ""}">${label}</a>`;
  return `<!doctype html><html lang="uk"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="mako-token" content="${esc(opts.token)}">${opts.refresh ? `<meta http-equiv="refresh" content="${opts.refresh}">` : ""}
<title>${esc(opts.title)} · Mako</title><link rel="stylesheet" href="/ui/style.css"></head><body>
<header><div class="bar"><span class="logo">🦈 Mako</span><nav>${tab("/", "inbox", "Вхідні")}${tab("/settings", "settings", "Налаштування")}</nav></div></header>
<main>${opts.body}</main>${opts.script ? `<script src="${opts.script}"></script>` : ""}</body></html>`;
}
