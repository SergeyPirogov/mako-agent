let version = "";

/** Shown in the header, so a stale build or an old running instance is easy to spot. */
export function setVersion(v: string): void {
  version = v;
}

export function esc(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}

/** Shared page shell for the inbox and settings pages. `token` protects every state-changing request. */
export function page(opts: {
  title: string;
  active: "inbox" | "settings" | "stats" | "tracker";
  token: string;
  body: string;
  script?: string;
  refresh?: number;
  /** Extra tags for <head>. */
  head?: string;
  /** Full-width main area (terminal). */
  wide?: boolean;
  /** Values for the page script, as data-* attributes on <body>. */
  data?: Record<string, string>;
  /** Orange favicon: an agent waits for the developer. */
  attention?: boolean;
  /** Without the header, for pages embedded in another Mako page. */
  bare?: boolean;
}): string {
  const tab = (href: string, id: string, label: string) =>
    `<a href="${href}" class="${opts.active === id ? "on" : ""}">${label}</a>`;
  return `<!doctype html><html lang="uk"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="mako-token" content="${esc(opts.token)}">${opts.refresh ? `<meta http-equiv="refresh" content="${opts.refresh}">` : ""}
<title>${esc(opts.title)} · Mako</title><link rel="stylesheet" href="/ui/style.css" id="look-css">
<script>/* the chosen look and theme, before the page paints */try{if(localStorage.getItem("mako-look")==="quiet")document.getElementById("look-css").href="/ui/style-quiet.css";var t=localStorage.getItem("mako-theme");if(t==="light"||t==="dark")document.documentElement.dataset.theme=t}catch(e){}</script>
<script src="/ui/dialog.js" defer></script>
<link rel="icon" type="image/png" href="/ui/favicon${opts.attention ? "-attention" : ""}.png"><link rel="apple-touch-icon" href="/ui/icon.png">${opts.head ?? ""}</head>
<body${Object.entries(opts.data ?? {}).map(([k, v]) => ` data-${k}="${esc(v)}"`).join("")}>
${opts.bare ? "" : `<header><div class="bar"><span class="logo"><span class="logo-mark">🦈 </span>Mako</span><nav>${tab("/", "inbox", "Задачі")}${tab("/tracker", "tracker", "Трекер")}${tab("/stats", "stats", "Статистика")}</nav><div class="right"><label class="look-pick" title="Вигляд Mako: спробуйте «Тихий» на всіх сторінках">Вигляд <select id="look-pick" onchange="try{localStorage.setItem('mako-look',this.value)}catch(e){};location.reload()"><option value="classic">Класичний</option><option value="quiet">Тихий</option></select></label><label class="look-pick" title="Світла чи темна тема; «як у системі» бере її з macOS/Windows">Тема <select id="theme-pick" onchange="try{localStorage.setItem('mako-theme',this.value)}catch(e){};if(this.value==='system')delete document.documentElement.dataset.theme;else document.documentElement.dataset.theme=this.value"><option value="system">як у системі</option><option value="light">світла</option><option value="dark">темна</option></select></label><script>try{document.getElementById("look-pick").value=localStorage.getItem("mako-look")==="quiet"?"quiet":"classic";var t=localStorage.getItem("mako-theme");document.getElementById("theme-pick").value=t==="light"||t==="dark"?t:"system"}catch(e){}</script>${version ? `<span class="ver" title="Версія Mako">${esc(version)}</span>` : ""}<nav>${tab("/settings", "settings", `<span aria-hidden="true">⚙</span><span class="nav-label"> Налаштування</span>`)}</nav></div></div></header>`}
<main class="${[opts.wide ? "wide" : "", opts.bare ? "bare" : ""].join(" ").trim()}">${opts.body}</main>${opts.script ? `<script src="${opts.script}"></script>` : ""}</body></html>`;
}
