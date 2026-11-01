package web

import (
	"embed"
	"encoding/base64"
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
)

//go:embed icons/*.png
var icons embed.FS

//go:embed sw.js
var serviceWorker []byte

const deviceCookie = "mako_device"

// remote: the request did not come from this Mac directly. Through `tailscale serve` it arrives from 127.0.0.1
// too, but with forwarding headers.
func remote(r *http.Request) bool {
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

// device: the paired phone a remote request comes from, nil when none.
func (s *Server) device(r *http.Request) *phone.Device {
	if s.Phones == nil {
		return nil
	}
	c, err := r.Cookie(deviceCookie)
	if err != nil {
		return nil
	}
	return s.Phones.ByToken(c.Value)
}

// What a phone may open without a key: the pairing and what installing the app needs.
func publicPath(p string) bool {
	return strings.HasPrefix(p, "/pair") || strings.HasPrefix(p, "/icons/") ||
		p == "/manifest.webmanifest" || p == "/sw.js" || p == "/style.css" || p == "/theme.js"
}

// Settings and pairing stay on the Mac: a phone gets the board and its buttons.
func macOnly(p string) bool {
	return p == "/settings" || strings.HasPrefix(p, "/api/settings") || strings.HasPrefix(p, "/api/phone") || p == "/api/show"
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
			page(w, http.StatusUnauthorized, "Phone not connected",
				"Open on the Mac: Mako → Settings → Phone, and scan the QR code with the iPhone camera.")
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

const pairPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mako</title><link rel="stylesheet" href="/style.css"></head><body style="display:block">
<div style="max-width:420px;margin:15vh auto;padding:0 20px;text-align:center"><div style="font-size:48px">🦈</div>
<h1 id="t" style="font-size:20px">Connecting…</h1><p id="m" style="color:var(--mute)"></p></div>
<script>
(function () {
  var code = new URLSearchParams(location.hash.slice(1)).get("code") || "";
  history.replaceState(null, "", "/pair");
  function fail(text) {
    document.getElementById("t").textContent = "Could not connect";
    document.getElementById("m").textContent = text;
  }
  if (!code) return fail("Scan the QR code from Mako on the Mac again.");
  fetch("/pair", {method: "POST", headers: {"Content-Type": "application/x-www-form-urlencoded"}, body: "code=" + encodeURIComponent(code)})
    .then(function (r) { return r.ok ? location.replace("/?paired=1") : r.text().then(fail); })
    .catch(function () { fail("Cannot reach Mako."); });
})();
</script></body></html>`

func setDeviceCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: deviceCookie, Value: token, Path: "/", MaxAge: 400 * 24 * 3600, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.Header.Get("X-Forwarded-Proto") == "https" || r.TLS != nil,
	})
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
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(serviceWorker)
	})
	// The app opens at start_url: with the device's key in it, so the installed app (whose storage iOS keeps
	// apart from Safari's) signs itself in.
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		start := "/"
		if d := s.device(r); d != nil {
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
	// The code rides in the link's #fragment, which a browser never sends: no history, server log or proxy
	// log (tailscale serve) sees it. This page hands it to POST /pair and clears it from the address bar.
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
		d, err := s.Phones.Pair(r.PostFormValue("code"), phone.DeviceName(r.UserAgent()))
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
		if err := json.NewDecoder(r.Body).Decode(&sub); err != nil || sub.Endpoint == "" {
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
		reply := map[string]any{"tailscale": t, "devices": s.Phones.Devices()}
		if t.Host != "" && t.Serving {
			reply["url"] = "https://" + t.Host
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
			t := phone.TailscaleState(r.Context(), port)
			if !t.Serving || t.Host == "" {
				err = fmt.Errorf("first open Mako for the phone through Tailscale")
				break
			}
			link := "https://" + t.Host + "/pair#code=" + url.QueryEscape(s.Phones.NewPairing())
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
