package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/phone"
	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
)

//go:embed icons/*.png
var icons embed.FS

//go:embed sw.js
var serviceWorker []byte

// The service worker carries the pairing page, to show it itself on a phone paired end to end.
func init() {
	page, _ := json.Marshal(pairPage)
	serviceWorker = bytes.Replace(serviceWorker, []byte(`/*pair page*/ ""`), page, 1)
}

const deviceCookie = "mako_device"

// remote: the request did not come from this Mac directly. Through `tailscale serve` it arrives from 127.0.0.1
// too, but with forwarding headers.
func remote(r *http.Request) bool {
	if tunneled(r) != nil {
		return true
	}
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "Forwarded", "Tailscale-User-Login"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return true
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// loopbackHost: Host names this Mac's loopback ("127.0.0.1:8080", "localhost", "[::1]:8080"), not a domain
// that resolves there.
func loopbackHost(h string) bool {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// device: the paired phone a remote request comes from, nil when none: the one whose sealed frame carried it, or
// the one whose key is in the cookie (paired the old way).
func (s *Server) device(r *http.Request) *phone.Device {
	if d := tunneled(r); d != nil {
		return d
	}
	if s.Phones == nil {
		return nil
	}
	c, err := r.Cookie(deviceCookie)
	if err != nil {
		return nil
	}
	return s.Phones.ByToken(c.Value)
}

// What a phone may open without a key: the pairing, what installing the app needs, and the tunnel, whose frames
// prove the device themselves.
func publicPath(p string) bool {
	return strings.HasPrefix(p, "/pair") || strings.HasPrefix(p, "/icons/") ||
		p == "/manifest.webmanifest" || p == "/favicon.ico" || p == "/sw.js" || p == "/style.css" || p == "/theme.js" || p == "/e2e"
}

// Settings and pairing stay on the Mac, and Terminal.app, Finder and the editor open there: a phone gets the board
// and its buttons.
func macOnly(p string) bool {
	return p == "/settings" || strings.HasPrefix(p, "/api/settings") || strings.HasPrefix(p, "/api/phone") || p == "/api/show" ||
		p == "/api/terminalapp" || p == "/api/open" || p == "/api/trybuild" || p == "/api/eval/run"
}

// guard lets this Mac in as is, and a phone only with a paired device's key.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !remote(r) {
			// DNS rebinding: a page on the attacker's domain reaches 127.0.0.1 with its own name in Host.
			if !loopbackHost(r.Host) {
				http.Error(w, "unexpected Host", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.device(r) == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "phone not paired", http.StatusUnauthorized)
				return
			}
			notConnected(w)
			return
		}
		if macOnly(r.URL.Path) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "only on the Mac", http.StatusForbidden)
				return
			}
			page(w, http.StatusForbidden, "Settings are on the Mac", "Mako's settings can only be changed on the Mac.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func page(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mako</title><link rel="stylesheet" href="/style.css"></head><body style="display:block">
<div style="max-width:420px;margin:15vh auto;padding:0 20px;text-align:center"><div style="font-size:48px">🦈</div>
<h1 style="font-size:20px">%s</h1><p style="color:var(--mute)">%s</p></div></body></html>`, html.EscapeString(title), html.EscapeString(text))
}

// notConnected: a phone with no key. Paired end to end, it has one only in its service worker: a page the worker did
// not load (opened before it took over) reloads once through it. The app from the Home Screen pairs on /pair.
func notConnected(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	io.WriteString(w, notConnectedPage)
}

const notConnectedPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mako</title><link rel="stylesheet" href="/style.css"></head><body style="display:block">
<div style="max-width:420px;margin:15vh auto;padding:0 20px;text-align:center"><div style="font-size:48px">🦈</div>
<h1 style="font-size:20px">Phone not connected</h1><p style="color:var(--mute)">Open on the Mac: Mako → Settings → Phone, and scan the QR code with the phone's camera.</p>
<p><a class="button primary" href="/pair">I have a link</a></p></div>
<script>
navigator.serviceWorker && navigator.serviceWorker.getRegistration().then(function (r) {
  try {
    if (!r || !r.active || navigator.serviceWorker.controller || sessionStorage.getItem("mako-reloaded")) return;
    sessionStorage.setItem("mako-reloaded", "1");
    location.reload();
  } catch (e) {}
});
</script></body></html>`

const pairPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mako</title><link rel="stylesheet" href="/style.css"></head><body style="display:block">
<div style="max-width:420px;margin:15vh auto;padding:0 20px;text-align:center"><div style="font-size:48px">🦈</div>
<h1 id="t" style="font-size:20px">Connecting…</h1><p id="m" style="color:var(--mute)"></p>
<form id="paste" hidden><p style="color:var(--mute)">Mako on the Mac → Settings → Phone → «Show the QR code» → «Copy the link», then paste it here.</p>
<input id="link" type="url" placeholder="https://…/pair#code=…" style="width:100%;box-sizing:border-box" autocomplete="off">
<p><button class="primary" type="submit">Connect</button></p></form></div>
<script>
(function () {
  // The fragment carries the code and k, the secret the device's end-to-end keys come from; a browser never sends
  // it. The service worker derives the keys, pairs with a sealed frame, keeps the keys in IndexedDB, and from then
  // on everything to Mako goes through it, sealed (docs/relay-e2e.md).
  var q = new URLSearchParams(location.hash.slice(1));
  history.replaceState(null, "", "/pair");
  function say(title, text) {
    document.getElementById("t").textContent = title;
    document.getElementById("m").textContent = text || "";
  }
  function fail(text) { say("Could not connect", text); }
  // Paired the old way, with the code alone: no service worker or no WebCrypto here.
  function plain(code) {
    fetch("/pair", {method: "POST", headers: {"Content-Type": "application/x-www-form-urlencoded"}, body: "code=" + encodeURIComponent(code)})
      .then(function (r) { return r.ok ? location.replace("/?paired=1") : r.text().then(fail); })
      .catch(function () { fail("Cannot reach Mako."); });
  }
  function sealed(code, k) {
    navigator.serviceWorker.register("/sw.js", {updateViaCache: "none"})
      .then(function () { return navigator.serviceWorker.ready; })
      .then(function (reg) {
        return new Promise(function (ok) {
          var ch = new MessageChannel();
          ch.port1.onmessage = function (e) { ok(e.data || {}); };
          reg.active.postMessage({t: "pair", code: code, k: k}, [ch.port2]);
        });
      })
      .then(function (r) { return r.ok ? location.replace("/?paired=1") : fail(r.error); })
      .catch(function (e) { fail("Cannot pair: " + (e && e.message || e)); });
  }
  function start(code, k) {
    if (!code) return fail("Scan the QR code from Mako on the Mac again.");
    say("Connecting…");
    if (k && navigator.serviceWorker && window.crypto && crypto.subtle && window.indexedDB) return sealed(code, k);
    plain(code);
  }
  if (q.get("code")) return start(q.get("code"), q.get("k"));
  // No link: the app from the Home Screen, which keeps its own storage apart from the browser's.
  say("Connect the phone");
  var form = document.getElementById("paste");
  form.hidden = false;
  form.onsubmit = function (e) {
    e.preventDefault();
    var u;
    try { u = new URL(document.getElementById("link").value.trim()); } catch (x) { return fail("That is not a link from Mako."); }
    if (u.host !== location.host) return fail("This link is for " + u.host + ", the app is at " + location.host + ".");
    var p = new URLSearchParams(u.hash.slice(1));
    form.hidden = true;
    start(p.get("code"), p.get("k"));
  };
})();
</script></body></html>`

func setDeviceCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: deviceCookie, Value: token, Path: "/", MaxAge: 400 * 24 * 3600, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil,
	})
}

// requestOrigin: the address the request was opened at ("https://host"), through `tailscale serve` too.
func requestOrigin(r *http.Request) string {
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	scheme := "http"
	if r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host
}

// relayURL: where phones reach this Mac through the relay, "" while it is off or its tunnel is down.
func (s *Server) relayURL() string {
	if s.Relay == nil {
		return ""
	}
	return strings.TrimRight(s.Relay(), "/")
}

// phoneURL: the address a phone opens Mako at: the relay's when its tunnel is up, else Tailscale's once served.
func (s *Server) phoneURL(t phone.Tailscale) string {
	if u := s.relayURL(); u != "" {
		return u
	}
	if t.Host != "" && t.Serving {
		return "https://" + t.Host
	}
	return ""
}

func (s *Server) phoneRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /icons/", func(w http.ResponseWriter, r *http.Request) {
		data, err := icons.ReadFile(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(data)
	})
	// Browsers ask for /favicon.ico on their own; answer with the app icon instead of a 404 in the console.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		data, err := icons.ReadFile("icons/icon-192.png")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(data)
	})
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(serviceWorker)
	})
	// The app opens at start_url: with the device's key in it, so the installed app (whose storage iOS keeps
	// apart from Safari's) signs itself in.
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		start := "/"
		if d := s.device(r); d != nil && !d.E2E {
			start = "/pair/d/" + d.Token
		}
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{
			"name": "Mako", "short_name": "Mako", "start_url": start, "scope": "/", "display": "standalone",
			"background_color": "#111214", "theme_color": "#111214", "lang": "en",
			"icons": []map[string]string{
				{"src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png"},
				{"src": "/icons/icon-512.png", "sizes": "512x512", "type": "image/png"},
			},
		})
	})
	// The code and the secret ride in the link's #fragment, which a browser never sends: no history, server log or
	// proxy log (tailscale serve, the relay) sees them. This page hands the code to POST /pair and clears both from
	// the address bar.
	mux.HandleFunc("GET /pair", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		io.WriteString(w, pairPage)
	})
	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil || !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// End to end: the phone sends a sealed pairing frame (JSON, version 1) and gets its device sealed back,
		// with no cookie the relay could read. Without it, the old way: the code in a form, the cookie in the reply.
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var f relay.Sealed
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&f); err != nil || f.V != relay.Version || f.T != relay.TypePair {
				http.Error(w, "bad pairing request", http.StatusBadRequest)
				return
			}
			_, reply, err := s.Phones.PairSealed(f, phone.DeviceName(r.UserAgent()), requestOrigin(r))
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			writeJSON(w, reply)
			return
		}
		d, err := s.Phones.Pair(r.PostFormValue("code"), phone.DeviceName(r.UserAgent()), requestOrigin(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		setDeviceCookie(w, r, d.Token)
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /pair/d/{token}", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil || s.Phones.ByToken(r.PathValue("token")) == nil {
			page(w, http.StatusUnauthorized, "Phone disconnected", "Connect it again: Mako on the Mac → Settings → Phone.")
			return
		}
		setDeviceCookie(w, r, r.PathValue("token"))
		http.Redirect(w, r, "/", http.StatusFound)
	})

	// The service worker Mako sends, as a hash: the board, which came sealed from the Mac, compares it with the
	// /sw.js the relay hands out in the clear (docs/relay-e2e.md, the limits).
	mux.HandleFunc("GET /api/shell", func(w http.ResponseWriter, r *http.Request) {
		sum := sha256.Sum256(serviceWorker)
		writeJSON(w, map[string]string{"sw": hex.EncodeToString(sum[:])})
	})

	// Pushes: the app subscribes with Mako's key after the user allows notifications.
	mux.HandleFunc("GET /api/push/key", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]string{"key": s.Phones.VAPIDPublic()})
	})
	mux.HandleFunc("POST /api/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		d := s.device(r)
		if d == nil || !sameOrigin(r) {
			http.Error(w, "phone not paired", http.StatusUnauthorized)
			return
		}
		var sub webpush.Subscription
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&sub); err != nil || sub.Endpoint == "" {
			http.Error(w, "bad subscription", http.StatusBadRequest)
			return
		}
		if err := s.Phones.Subscribe(d.Token, &sub); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	// The «Phone» section of the settings, on the Mac.
	mux.HandleFunc("GET /api/phone", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil {
			http.NotFound(w, r)
			return
		}
		t := phone.TailscaleState(r.Context(), s.D.Config().Notifications.Port)
		type deviceView struct {
			phone.Device
			ViaTailscale bool `json:"viaTailscale"`
		}
		devices := []deviceView{}
		for _, d := range s.Phones.Devices() {
			devices = append(devices, deviceView{d, d.ViaTailscale()})
		}
		reply := map[string]any{"tailscale": t, "devices": devices, "tailscaleLeftovers": s.Phones.HasTailscaleDevices()}
		if u := s.phoneURL(t); u != "" {
			reply["url"] = u
		}
		if u := s.relayURL(); u != "" {
			reply["relay"] = map[string]string{"url": u}
		}
		writeJSON(w, reply)
	})
	mux.HandleFunc("POST /api/phone/{action}", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil || !sameOrigin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		port := s.D.Config().Notifications.Port
		var err error
		reply := map[string]any{"ok": true}
		switch r.PathValue("action") {
		case "serve":
			err = phone.Serve(r.Context(), port, r.URL.Query().Get("public") == "1")
		case "unserve":
			err = phone.Unserve(r.Context())
		case "pairing":
			base := s.relayURL()
			if base == "" {
				base = s.phoneURL(phone.TailscaleState(r.Context(), port))
			}
			if base == "" {
				err = fmt.Errorf("first open Mako for the phone through Tailscale")
				break
			}
			code, secret := s.Phones.NewPairingKey()
			link := base + "/pair#code=" + url.QueryEscape(code) + "&k=" + url.QueryEscape(secret)
			png, qerr := qrcode.Encode(link, qrcode.Medium, 512)
			if qerr != nil {
				err = qerr
				break
			}
			reply["url"] = link
			reply["qr"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		case "remove":
			s.Phones.Remove(r.URL.Query().Get("id"))
		case "test":
			errs := s.Phones.Push(r.Context(), phone.Message{Title: "🦈 Mako", Body: i18n.T("notify.phoneWorks"), URL: "/"})
			if len(errs) > 0 {
				err = errs[0]
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			reply = map[string]any{"ok": false, "error": err.Error()}
		}
		writeJSON(w, reply)
	})
}
