// Mako's service worker: shows pushes, and a tap opens the task on the board. A notification's buttons answer
// the agent right away; a push with close takes an answered request's notification away.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

self.addEventListener("push", (e) => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch { m = { title: "Mako", body: e.data?.text() }; }
  if (m.close && m.tag) {
    // A push has to show a notification: the same tag, quietly, closed at once, takes the old one with it.
    e.waitUntil((async () => {
      await self.registration.showNotification(m.title || "Mako", { body: m.body || "", tag: m.tag, silent: true, icon: "/icons/icon-192.png" });
      for (const n of await self.registration.getNotifications({ tag: m.tag })) n.close();
    })());
    return;
  }
  const actions = m.actions || [];
  const max = self.Notification?.maxActions ?? 2;
  e.waitUntil(self.registration.showNotification(m.title || "Mako", {
    body: m.body || "",
    tag: m.tag || undefined,
    renotify: !!m.tag,
    icon: "/icons/icon-192.png",
    badge: "/icons/badge-96.png", // Android draws it as a white mask in the status bar
    actions: actions.slice(0, max).map((a, i) => ({ action: String(i), title: a.title })),
    data: { url: m.url || "/", actions },
  }));
});

async function openBoard(url) {
  const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  for (const c of all) {
    if (new URL(c.url).origin === self.location.origin) {
      await c.focus();
      return c.navigate(url);
    }
  }
  return self.clients.openWindow(url);
}

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = new URL(e.notification.data?.url || "/", self.location.origin).href;
  const action = e.action ? e.notification.data?.actions?.[Number(e.action)] : null;
  if (!action) {
    e.waitUntil(openBoard(url));
    return;
  }
  // A button: the answer goes to the agent; only when it cannot (already answered, agent gone) the board opens.
  e.waitUntil((async () => {
    try {
      const res = await fetch(new URL(action.url, self.location.origin).href, { method: "POST", credentials: "same-origin" });
      if ((await res.json()).ok) return;
    } catch {}
    return openBoard(url);
  })());
});
