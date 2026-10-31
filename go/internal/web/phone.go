package web

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
	qrcode "github.com/skip2/go-qrcode"

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
			page(w, http.StatusUnauthorized, "Телефон не підключено",
				"Відкрийте на Mac: Mako → Налаштування → Телефон, і відскануйте QR-код камерою iPhone.")
			return
		}
		if macOnly(r.URL.Path) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "only on the Mac", http.StatusForbidden)
				return
			}
			page(w, http.StatusForbidden, "Налаштування — на Mac", "Змінити налаштування Mako можна лише на Mac.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func page(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="uk"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mako</title><link rel="stylesheet" href="/style.css"></head><body style="display:block">
<div style="max-width:420px;margin:15vh auto;padding:0 20px;text-align:center"><div style="font-size:48px">🦈</div>
<h1 style="font-size:20px">%s</h1><p style="color:var(--mute)">%s</p></div></body></html>`, html.EscapeString(title), html.EscapeString(text))
}

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
			"background_color": "#111214", "theme_color": "#111214", "lang": "uk",
			"icons": []map[string]string{
				{"src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png"},
				{"src": "/icons/icon-512.png", "sizes": "512x512", "type": "image/png"},
			},
		})
	})
	mux.HandleFunc("GET /pair", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil {
			http.NotFound(w, r)
			return
		}
		d, err := s.Phones.Pair(r.URL.Query().Get("code"), phone.DeviceName(r.UserAgent()))
		if err != nil {
			page(w, http.StatusUnauthorized, "Не вдалося підключити", err.Error())
			return
		}
		setDeviceCookie(w, r, d.Token)
		http.Redirect(w, r, "/?paired=1", http.StatusFound)
	})
	mux.HandleFunc("GET /pair/d/{token}", func(w http.ResponseWriter, r *http.Request) {
		if s.Phones == nil || s.Phones.ByToken(r.PathValue("token")) == nil {
			page(w, http.StatusUnauthorized, "Телефон відключено", "Підключіть його знову: Mako на Mac → Налаштування → Телефон.")
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

	// The «Телефон» section of the settings, on the Mac.
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
				err = fmt.Errorf("спершу відкрийте Mako для телефона через Tailscale")
				break
			}
			link := "https://" + t.Host + "/pair?code=" + url.QueryEscape(s.Phones.NewPairing())
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
			errs := s.Phones.Push(r.Context(), phone.Message{Title: "🦈 Mako", Body: "Сповіщення на телефон працюють", URL: "/"})
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
