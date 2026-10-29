// Desktop app: Mako in the menu bar (tray) with its own window and native notifications.
import { app, BrowserWindow, Menu, Notification, Tray, dialog, nativeImage, shell, type MenuItemConstructorOptions } from "electron";
import { execFileSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { startMako, type Runtime } from "./runtime.js";
import { setNotificationHost, type Decision } from "./notify.js";
import { sessions, sessionIdFor, attentionSessions, type SessionStatus } from "./sessions.js";
import type { TaskRecord } from "./state.js";
import { log } from "./log.js";

const ASSETS = fileURLToPath(new URL("../assets/", import.meta.url));
const isMac = process.platform === "darwin";

const SESSION_LABELS: Record<SessionStatus, string> = {
  starting: "запуск",
  working: "працює",
  waiting: "чекає на вас",
  idle: "закінчив хід",
  exited: "публікується",
};

app.setName("Mako");
if (!app.requestSingleInstanceLock()) app.quit();
else void main();

async function main(): Promise<void> {
  fixPath();
  // Config, .env and state live in the app's own folder: ~/Library/Application Support/Mako on macOS.
  const home = process.env.MAKO_HOME ?? app.getPath("userData");
  mkdirSync(home, { recursive: true });
  process.chdir(home);

  await app.whenReady();
  if (isMac) app.dock?.hide();

  let win: BrowserWindow | null = null;
  const notifications = new Set<Notification>(); // keep references so they are not garbage-collected

  setNotificationHost({
    ask: (title, body, timeoutSec) =>
      new Promise<Decision>((resolve) => {
        if (!Notification.isSupported()) return resolve("ignored");
        const n = new Notification({
          title,
          body,
          actions: [
            { type: "button", text: "Взяти" },
            { type: "button", text: "Відкласти" },
          ],
          timeoutType: "never",
        });
        let settled = false;
        const settle = (d: Decision) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          notifications.delete(n);
          resolve(d);
        };
        const timer = setTimeout(() => {
          n.close();
          settle("ignored");
        }, timeoutSec * 1000);
        n.on("action", (_e, index) => settle(index === 0 ? "take" : "later"));
        // Clicking the body opens the inbox; the task keeps its slot until answered there or timed out.
        n.on("click", () => openWindow("/"));
        n.on("close", () => settle("ignored"));
        notifications.add(n);
        n.show();
      }),
    inform(title, body, url) {
      if (!Notification.isSupported()) return;
      const n = new Notification({ title, body });
      n.on("click", () => (url ? openUrl(url) : openWindow("/")));
      n.on("close", () => notifications.delete(n));
      notifications.add(n);
      n.show();
    },
    openUrl,
  });

  let runtime: Runtime;
  try {
    runtime = startMako({ configPath: "mako.config.json", envPath: ".env", openSettingsOnFirstRun: false });
  } catch (err: any) {
    dialog.showErrorBox("Mako не запустився", err.message);
    app.exit(1);
    return;
  }
  log("desktop", `data folder: ${home}`);

  const local = (url: string) => Boolean(runtime.port) && url.startsWith(`http://127.0.0.1:${runtime.port}/`);

  function openUrl(url: string): void {
    if (local(url)) openWindow(new URL(url).pathname);
    else void shell.openExternal(url);
  }

  function openWindow(pagePath = "/"): void {
    const url = runtime.url(pagePath);
    if (!url) return;
    if (!win || win.isDestroyed()) {
      win = new BrowserWindow({
        width: 1180,
        height: 800,
        minWidth: 420,
        minHeight: 400,
        title: "Mako",
        icon: path.join(ASSETS, "icon.png"),
        show: false,
        webPreferences: { contextIsolation: true, sandbox: true, nodeIntegration: false },
      });
      const w = win;
      // Tracker issues and PRs open in the default browser; Mako's own pages stay in the window.
      w.webContents.setWindowOpenHandler(({ url: target }) => {
        if (local(target)) void w.loadURL(target);
        else void shell.openExternal(target);
        return { action: "deny" };
      });
      w.webContents.on("will-navigate", (e, target) => {
        if (!local(target)) {
          e.preventDefault();
          void shell.openExternal(target);
        }
      });
      // The settings page asks before leaving with unsaved changes; Electron needs an explicit dialog for that.
      w.webContents.on("will-prevent-unload", (e) => {
        const choice = dialog.showMessageBoxSync(w, {
          type: "question",
          buttons: ["Закрити без збереження", "Залишитись"],
          defaultId: 1,
          cancelId: 1,
          message: "Є незбережені зміни в налаштуваннях",
        });
        if (choice === 0) e.preventDefault();
      });
      w.once("ready-to-show", () => w.show());
      w.on("focus", () => w.flashFrame(false));
      w.on("closed", () => {
        win = null;
        if (isMac) app.dock?.hide();
      });
      if (isMac) void app.dock?.show();
    }
    void win.loadURL(url);
    win.show();
    win.focus();
  }

  // ---- tray ----

  const icons = {
    normal: trayIcon(false),
    attention: trayIcon(true),
  };
  const tray = new Tray(icons.normal);
  tray.setToolTip("Mako");
  if (!isMac) tray.on("click", () => openWindow("/"));
  let lastSignature = "";
  let lastAttention = false;

  function refreshTray(): void {
    const view = trayView();
    const signature = JSON.stringify(view);
    if (signature === lastSignature) return;
    lastSignature = signature;
    tray.setToolTip(`Mako · ${view.headline}`);
    tray.setImage(view.attention ? icons.attention : icons.normal);
    if (isMac) tray.setTitle(view.title, { fontType: "monospacedDigit" });
    // A new reason for attention: make an open but unfocused window stand out (Dock bounce, taskbar flash).
    if (view.attention && !lastAttention && win && !win.isDestroyed() && !win.isFocused()) win.flashFrame(true);
    lastAttention = view.attention;
    tray.setContextMenu(Menu.buildFromTemplate(menuTemplate(view)));
  }

  interface TrayItem {
    id: string;
    label: string;
    page: string;
    attention: boolean;
  }

  function trayView() {
    const o = runtime.app.orchestrator;
    const cfg = runtime.app.cfg;
    if (!o || !cfg) {
      return { configured: false, headline: runtime.app.problem ?? "не налаштований", title: "", attention: true, paused: false, items: [] as TrayItem[], newCount: 0, loginItem: loginItem() };
    }
    const records = o.state.all();
    const items: TrayItem[] = records
      .filter((r) => r.status === "queued" || r.status === "running" || r.status === "handed-off")
      .map((r) => itemFor(r))
      .sort((a, b) => Number(b.attention) - Number(a.attention));
    const newCount = records.filter((r) => r.status === "asking").length;
    const waiting = attentionSessions().length > 0;
    const wip = o.wip();
    const count = cfg.wipLimit ? `${wip}/${cfg.wipLimit}` : wip ? String(wip) : "";
    const attention = waiting || newCount > 0;
    return {
      configured: true,
      headline: o.paused ? "на паузі" : `в роботі ${cfg.wipLimit ? `${wip} з ${cfg.wipLimit}` : wip}`,
      title: o.paused ? "⏸" : `${attention ? "● " : ""}${count}`.trim(),
      attention,
      paused: o.paused,
      items,
      newCount,
      loginItem: loginItem(),
    };
  }

  function itemFor(r: TaskRecord): TrayItem {
    const session = sessions.get(sessionIdFor(r.task));
    const status = session && r.status === "running"
      ? SESSION_LABELS[session.status]
      : r.status === "queued" ? "у черзі" : r.status === "handed-off" ? "чекає PR" : "виконується";
    const title = r.task.title.length > 40 ? r.task.title.slice(0, 39) + "…" : r.task.title;
    const attention = Boolean(session && r.status === "running" && (session.status === "waiting" || session.status === "idle"));
    return {
      id: r.id,
      label: `${attention ? "● " : ""}${r.id}  ${title} — ${status}${attention && session?.message ? `: ${session.message.slice(0, 50)}` : ""}`,
      page: session && r.status === "running" ? `/session/${session.id}` : "/",
      attention,
    };
  }

  function menuTemplate(view: ReturnType<typeof trayView>): MenuItemConstructorOptions[] {
    const o = runtime.app.orchestrator;
    const items: MenuItemConstructorOptions[] = [
      { label: view.configured ? `Mako · ${view.headline}` : `Mako · ${view.headline}`, enabled: false },
    ];
    if (!view.configured) items.push({ label: "Налаштувати…", click: () => openWindow("/settings") });
    if (view.newCount) items.push({ label: `Нові задачі: ${view.newCount}`, click: () => openWindow("/") });
    if (view.items.length) {
      items.push({ type: "separator" });
      for (const it of view.items) items.push({ label: it.label, click: () => openWindow(it.page) });
    }
    items.push(
      { type: "separator" },
      { label: "Відкрити Mako", accelerator: isMac ? "Cmd+O" : undefined, click: () => openWindow("/") },
      { label: "Налаштування…", accelerator: isMac ? "Cmd+," : undefined, click: () => openWindow("/settings") },
      { type: "separator" },
      {
        label: "Пауза: не брати нових задач",
        type: "checkbox",
        checked: view.paused,
        enabled: Boolean(o),
        click: (item) => {
          if (o) o.paused = item.checked;
          log("desktop", item.checked ? "paused" : "resumed");
          refreshTray();
        },
      },
    );
    if (process.platform !== "linux") {
      items.push({
        label: "Запускати при вході в систему",
        type: "checkbox",
        checked: view.loginItem,
        click: (item) => app.setLoginItemSettings({ openAtLogin: item.checked }),
      });
    }
    items.push(
      { label: "Показати папку даних", click: () => void shell.openPath(home) },
      { type: "separator" },
      { label: "Вийти з Mako", accelerator: isMac ? "Cmd+Q" : undefined, click: () => app.quit() },
    );
    return items;
  }

  function loginItem(): boolean {
    return process.platform === "linux" ? false : app.getLoginItemSettings().openAtLogin;
  }

  refreshTray();
  setInterval(refreshTray, 1500);
  // Handle used by the automated UI test; harmless otherwise.
  (globalThis as any).makoDesktop = { tray, menu: () => menuTemplate(trayView()), view: trayView, openWindow };

  if (!runtime.app.orchestrator) openWindow("/settings");

  app.on("second-instance", () => openWindow("/"));
  app.on("activate", () => openWindow("/"));
  // Closing the window keeps Mako running in the tray.
  app.on("window-all-closed", () => {});

  let quitting = false;
  app.on("before-quit", (e) => {
    if (quitting) return;
    const running = [...sessions.values()].filter((s) => s.status !== "exited").length;
    if (running) {
      const choice = dialog.showMessageBoxSync({
        type: "warning",
        buttons: ["Вийти й зупинити", "Скасувати"],
        defaultId: 1,
        cancelId: 1,
        message: `Агенти ще працюють (${running}). Вийти й зупинити їх?`,
        detail: "Задачі повернуться в чергу при наступному запуску, зміни у worktree збережуться.",
      });
      if (choice === 1) return e.preventDefault();
    }
    e.preventDefault();
    quitting = true;
    void runtime.shutdown().finally(() => app.exit(0));
  });
}

function trayIcon(attention: boolean): Electron.NativeImage {
  if (isMac) {
    // Normally a template image (macOS tints it for light and dark menu bars); orange while an agent waits for you.
    if (attention) return nativeImage.createFromPath(path.join(ASSETS, "trayAttention.png"));
    const img = nativeImage.createFromPath(path.join(ASSETS, "trayTemplate.png"));
    img.setTemplateImage(true);
    return img;
  }
  return nativeImage.createFromPath(path.join(ASSETS, attention ? "tray-attention.png" : "tray.png"));
}

/** Apps started from Finder get a minimal PATH; take the user's login shell PATH so claude, gh and git are found. */
function fixPath(): void {
  if (process.platform === "win32") return;
  try {
    const shellPath = process.env.SHELL || (isMac ? "/bin/zsh" : "/bin/bash");
    const out = execFileSync(shellPath, ["-ilc", 'printf "__MAKO_PATH__%s__MAKO_PATH__" "$PATH"'], {
      encoding: "utf8",
      timeout: 5000,
      stdio: ["ignore", "pipe", "ignore"],
    });
    const found = out.match(/__MAKO_PATH__(.*)__MAKO_PATH__/)?.[1];
    if (found) process.env.PATH = found;
  } catch {
    // keep the inherited PATH
  }
}
