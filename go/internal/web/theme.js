// Shared by the board and the settings: the words (English only), the confirmation dialog and the theme button in the header.

// I18N: the pages' words (GET /api/i18n); t picks one, {name} filled from vars.
let I18N = {};
// LANG: the locale of dates and times.
const LANG = "en";
function t(key, vars = {}) {
  return String(I18N[key] ?? key).replace(/\{(\w+)\}/g, (m, k) => (k in vars ? vars[k] : m));
}
// loadI18n fetches the words and puts them into the static page: data-t is an
// element's HTML, data-t-title its title, data-t-ph its placeholder.
async function loadI18n() {
  try {
    const r = await (await fetch("/api/i18n", { cache: "no-store" })).json();
    I18N = r.strings ?? {};
  } catch {}
  document.documentElement.lang = LANG;
  for (const el of document.querySelectorAll("[data-t]")) el.innerHTML = t(el.dataset.t);
  for (const el of document.querySelectorAll("[data-t-title]")) el.title = t(el.dataset.tTitle);
  for (const el of document.querySelectorAll("[data-t-ph]")) el.placeholder = t(el.dataset.tPh);
  window.dispatchEvent(new Event("mako-i18n"));
  return LANG;
}

// askConfirm: Mako's own confirmation instead of the window's confirm(): resolves true on OK, false on Cancel, Esc
// or a click outside. Focus starts on OK (on Cancel when danger, so a stray Enter does not destroy anything); a
// destructive OK is red. {ok, cancel} are the buttons' labels.
function askConfirm(message, { ok = t("b.confirm.ok"), cancel = t("b.confirm.cancel"), danger = false } = {}) {
  return new Promise((resolve) => {
    const dlg = document.createElement("dialog");
    dlg.className = "confirm-dlg";
    dlg.setAttribute("aria-label", message);
    const text = document.createElement("p");
    text.textContent = message;
    const actions = document.createElement("div");
    actions.className = "confirm-a";
    const okBtn = document.createElement("button"), noBtn = document.createElement("button");
    okBtn.type = noBtn.type = "button";
    okBtn.textContent = ok; noBtn.textContent = cancel;
    okBtn.className = danger ? "danger" : "primary";
    // The buttons and a click outside answer at once; Esc closes the dialog, and that is a cancel.
    let done = false;
    const answer = (yes) => { if (done) return; done = true; resolve(yes); dlg.remove(); };
    okBtn.onclick = () => answer(true);
    noBtn.onclick = () => answer(false);
    actions.append(noBtn, okBtn);
    const box = document.createElement("div");
    box.className = "confirm-box";
    box.append(text, actions);
    dlg.append(box);
    dlg.addEventListener("click", (e) => { if (e.target === dlg) answer(false); });
    dlg.addEventListener("close", () => answer(false));
    document.body.append(dlg);
    dlg.showModal();
    (danger ? noBtn : okBtn).focus();
  });
}

(() => {
const ICONS = {
  auto: '<svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="1.5"/><path d="M8 2a6 6 0 0 1 0 12z" fill="currentColor"/></svg>',
  light: '<svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="3" fill="currentColor"/><g stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3 3l1.4 1.4M11.6 11.6 13 13M3 13l1.4-1.4M11.6 4.4 13 3"/></g></svg>',
  dark: '<svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true"><path d="M13.5 10.2A6 6 0 0 1 5.8 2.5a6 6 0 1 0 7.7 7.7z" fill="currentColor"/></svg>',
};
const THEMES = ["auto", "light", "dark"];
function applyTheme(t) {
  if (t === "auto") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.dataset.theme = t;
  document.getElementById("theme").innerHTML = ICONS[t];
  document.getElementById("theme").title = window.t(`theme.${t}`);
}
let theme = (() => { try { return localStorage.getItem("mako-theme") || "auto"; } catch { return "auto"; } })();
if (!THEMES.includes(theme)) theme = "auto";
applyTheme(theme);
window.addEventListener("mako-i18n", () => applyTheme(theme));
document.getElementById("theme").onclick = () => {
  theme = THEMES[(THEMES.indexOf(theme) + 1) % THEMES.length];
  try { localStorage.setItem("mako-theme", theme); } catch {}
  applyTheme(theme);
};

})();
