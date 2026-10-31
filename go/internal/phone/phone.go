// Package phone lets an iPhone use Mako: the board installed on its home screen (a web app), reached over
// Tailscale with HTTPS, paired once with a QR code, and push notifications to it.
package phone

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Device is a paired phone: its token is its key to the board, its subscription where its pushes go.
type Device struct {
	ID       string                `json:"id"`
	Name     string                `json:"name"`
	Token    string                `json:"token"`
	Created  int64                 `json:"created"`
	LastSeen int64                 `json:"lastSeen"`
	Push     *webpush.Subscription `json:"push,omitempty"`
}

type stored struct {
	VAPIDPublic  string    `json:"vapidPublic"`
	VAPIDPrivate string    `json:"vapidPrivate"`
	Devices      []*Device `json:"devices"`
}

// Phones keeps the paired devices and the push keys in <state>/phones.json.
type Phones struct {
	mu      sync.Mutex
	file    string
	data    stored
	pairing map[string]time.Time // one-time codes from the QR, valid for 10 minutes
	// Send delivers a push; tests replace it.
	Send func(ctx context.Context, msg []byte, sub *webpush.Subscription, o *webpush.Options) (*http.Response, error)
}

func Open(stateDir string) (*Phones, error) {
	p := &Phones{file: filepath.Join(stateDir, "phones.json"), pairing: map[string]time.Time{}, Send: webpush.SendNotificationWithContext}
	if data, err := os.ReadFile(p.file); err == nil {
		if err := json.Unmarshal(data, &p.data); err != nil {
			return nil, fmt.Errorf("phones.json: %w", err)
		}
	}
	if p.data.VAPIDPrivate == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		p.data.VAPIDPrivate, p.data.VAPIDPublic = priv, pub
		if err := p.saveLocked(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Phones) saveLocked() error {
	data, _ := json.MarshalIndent(p.data, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p.file), 0o755); err != nil {
		return err
	}
	tmp := p.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.file)
}

func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// VAPIDPublic: the key the phone subscribes to pushes with.
func (p *Phones) VAPIDPublic() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.data.VAPIDPublic
}

// NewPairing: a one-time code for the QR, valid for 10 minutes.
func (p *Phones) NewPairing() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for code, until := range p.pairing {
		if now.After(until) {
			delete(p.pairing, code)
		}
	}
	code := random(18)
	p.pairing[code] = now.Add(10 * time.Minute)
	return code
}

// Pair turns a QR code into a device: the code works once.
func (p *Phones) Pair(code, name string) (*Device, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.pairing[code]
	if !ok || time.Now().After(until) {
		return nil, errors.New("код застарів: відкрийте «Телефон» у налаштуваннях Mako на Mac і відскануйте новий QR-код")
	}
	delete(p.pairing, code)
	now := time.Now().UnixMilli()
	d := &Device{ID: random(6), Name: name, Token: random(32), Created: now, LastSeen: now}
	p.data.Devices = append(p.data.Devices, d)
	return d, p.saveLocked()
}

// ByToken: the paired device with this token, nil when there is none.
func (p *Phones) ByToken(token string) *Device {
	if token == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.data.Devices {
		if subtle.ConstantTimeCompare([]byte(d.Token), []byte(token)) == 1 {
			if now := time.Now().UnixMilli(); now-d.LastSeen > 60_000 {
				d.LastSeen = now
				p.saveLocked()
			}
			c := *d
			return &c
		}
	}
	return nil
}

// Devices: the paired phones, newest first, without their tokens.
func (p *Phones) Devices() []Device {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []Device{}
	for _, d := range p.data.Devices {
		c := *d
		c.Token = ""
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

func (p *Phones) Remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.data.Devices[:0]
	for _, d := range p.data.Devices {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	p.data.Devices = kept
	p.saveLocked()
}

// Subscribe keeps where the device's pushes go (nil: no more pushes).
func (p *Phones) Subscribe(token string, sub *webpush.Subscription) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.data.Devices {
		if d.Token == token {
			d.Push = sub
			return p.saveLocked()
		}
	}
	return errors.New("пристрій не підключено")
}

// Message is what a push says; the app shows title and body and opens url on a tap. Its actions are buttons that
// answer right from the notification (POST to their url); Close takes the notification with Tag away instead.
type Message struct {
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	URL     string   `json:"url,omitempty"`
	Tag     string   `json:"tag,omitempty"`
	Actions []Action `json:"actions,omitempty"`
	Close   bool     `json:"close,omitempty"`
}

// Action is a button of a pushed notification.
type Action struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Push sends a message to every subscribed phone. A subscription the push service no longer knows (404, 410:
// the app was removed or its permission taken back) is dropped.
func (p *Phones) Push(ctx context.Context, m Message) []error {
	payload, _ := json.Marshal(m)
	p.mu.Lock()
	targets := []*Device{}
	for _, d := range p.data.Devices {
		if d.Push != nil {
			targets = append(targets, d)
		}
	}
	opts := &webpush.Options{
		Subscriber:      "https://github.com/SergeyPirogov/mako-agent",
		VAPIDPublicKey:  p.data.VAPIDPublic,
		VAPIDPrivateKey: p.data.VAPIDPrivate,
		TTL:             3600,
		Urgency:         webpush.UrgencyHigh,
	}
	p.mu.Unlock()
	var errs []error
	for _, d := range targets {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		res, err := p.Send(cctx, payload, d.Push, opts)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
			continue
		}
		res.Body.Close()
		switch {
		case res.StatusCode == 404 || res.StatusCode == 410:
			p.Subscribe(d.Token, nil)
			errs = append(errs, fmt.Errorf("%s: підписка на сповіщення більше не діє", d.Name))
		case res.StatusCode >= 300:
			errs = append(errs, fmt.Errorf("%s: сервіс сповіщень відповів %d", d.Name, res.StatusCode))
		}
	}
	return errs
}

// DeviceName: «iPhone» and friends from a User-Agent.
func DeviceName(userAgent string) string {
	for _, n := range []string{"iPhone", "iPad", "Android", "Macintosh"} {
		if strings.Contains(userAgent, n) {
			if n == "Macintosh" {
				return "Mac"
			}
			return n
		}
	}
	return "Пристрій"
}
