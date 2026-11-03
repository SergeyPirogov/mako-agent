// Package phone lets an iPhone or Android phone use Mako: the board installed on its home screen (a web app), reached over
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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/SergeyPirogov/mako-agent/go/internal/relay"
)

// Device is a paired phone: its token is its key to the board, its subscription where its pushes go.
type Device struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Token    string `json:"token"`
	Created  int64  `json:"created"`
	LastSeen int64  `json:"lastSeen"`
	// Origin is the address the phone was paired through ("https://host"). Cookies belong to an origin, so a phone
	// paired through another address has to be paired again. Empty: paired before Mako kept it, which was always
	// through Tailscale.
	Origin string                `json:"origin,omitempty"`
	Push   *webpush.Subscription `json:"push,omitempty"`
	// The end-to-end keys through the relay, derived from the QR's secret (empty: paired before there were any),
	// and their counters: SendN is the highest counter reserved for the Mac's frames, RecvN the last of the
	// phone's frames accepted. Both are written down before use, so a restart neither reuses a nonce nor lets an
	// old frame in again.
	KeyP2M []byte `json:"keyP2M,omitempty"`
	KeyM2P []byte `json:"keyM2P,omitempty"`
	SendN  uint64 `json:"sendN,omitempty"`
	RecvN  uint64 `json:"recvN,omitempty"`
}

// pending is a QR code not used yet: until when it works, and the secret the device's keys come from.
type pending struct {
	until  time.Time
	secret []byte
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
	pairing map[string]pending // one-time codes from the QR, valid for 10 minutes
	// Send delivers a push; tests replace it.
	Send func(ctx context.Context, msg []byte, sub *webpush.Subscription, o *webpush.Options) (*http.Response, error)
}

func Open(stateDir string) (*Phones, error) {
	p := &Phones{file: filepath.Join(stateDir, "phones.json"), pairing: map[string]pending{}, Send: webpush.SendNotificationWithContext}
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
	code, _ := p.NewPairingKey()
	return code
}

// NewPairingKey: a one-time code for the QR, valid for 10 minutes, and the secret (base64url) the device's
// end-to-end keys are derived from. Both go into the link's #fragment; the secret never comes back to the Mac.
func (p *Phones) NewPairingKey() (code, secret string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for c, pend := range p.pairing {
		if now.After(pend.until) {
			delete(p.pairing, c)
		}
	}
	key := make([]byte, 32)
	rand.Read(key)
	code = random(18)
	p.pairing[code] = pending{until: now.Add(10 * time.Minute), secret: key}
	return code, base64.RawURLEncoding.EncodeToString(key)
}

// ViaTailscale: the phone was paired through Tailscale (a *.ts.net address, or an old phones.json with no origin).
func (d Device) ViaTailscale() bool {
	if d.Origin == "" {
		return true
	}
	u, err := url.Parse(d.Origin)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Hostname()), ".ts.net")
}

// HasTailscaleDevices: some paired phone came through Tailscale, so a Funnel or `tailscale serve` of an older
// Mako may still be open.
func (p *Phones) HasTailscaleDevices() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.data.Devices {
		if d.ViaTailscale() {
			return true
		}
	}
	return false
}

var errExpired = errors.New("code expired: open \"Phone\" in the Mako settings on the Mac and scan a new QR code")

// keysLocked: the device keys of a QR code still valid.
func (p *Phones) keysLocked(code string) (relay.Keys, error) {
	pend, ok := p.pairing[code]
	if !ok || time.Now().After(pend.until) {
		return relay.Keys{}, errExpired
	}
	return relay.DeriveKeys(pend.secret, []byte(code))
}

func (p *Phones) addLocked(code, name, origin string, k relay.Keys) (*Device, error) {
	delete(p.pairing, code)
	now := time.Now().UnixMilli()
	d := &Device{ID: random(6), Name: name, Token: random(32), Created: now, LastSeen: now, Origin: origin,
		KeyP2M: k.P2M, KeyM2P: k.M2P}
	p.data.Devices = append(p.data.Devices, d)
	return d, p.saveLocked()
}

// Pair turns a QR code into a device: the code works once. origin is the address the phone opened. The device
// keeps the keys derived from the QR's secret.
func (p *Phones) Pair(code, name, origin string) (*Device, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k, err := p.keysLocked(code)
	if err != nil {
		return nil, err
	}
	return p.addLocked(code, name, origin, k)
}

// PairSealed pairs from the phone's sealed pairing frame (its code in f.I) and seals the reply. A frame that does
// not decrypt leaves the code valid, so a relay slipping in garbage cannot spoil the pairing. name is used when
// the phone sends none.
func (p *Phones) PairSealed(f relay.Sealed, name, origin string) (*Device, relay.Sealed, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k, err := p.keysLocked(f.I)
	if err != nil {
		return nil, relay.Sealed{}, err
	}
	mac := relay.MacID(origin)
	req, err := relay.OpenPair(k, mac, f)
	if err != nil {
		return nil, relay.Sealed{}, errors.New("the pairing request does not decrypt: scan the QR code again")
	}
	if n := strings.TrimSpace(req.Name); n != "" {
		name = n
		if r := []rune(name); len(r) > 40 {
			name = string(r[:40])
		}
	}
	d, err := p.addLocked(f.I, name, origin, k)
	if err != nil {
		return nil, relay.Sealed{}, err
	}
	reply, err := relay.SealPairReply(k, mac, f.I, relay.PairReply{ID: d.ID, Nonce: req.Nonce})
	return d, reply, err
}

var errNoKeys = errors.New("the phone has no end-to-end keys: pair it again")

func (p *Phones) byIDLocked(id string) (*Device, error) {
	for _, d := range p.data.Devices {
		if d.ID == id {
			if len(d.KeyP2M) == 0 || len(d.KeyM2P) == 0 {
				return nil, errNoKeys
			}
			return d, nil
		}
	}
	return nil, errors.New("device removed")
}

// Sealer seals the Mac's frames to the device id. The counters it reserves are written to phones.json first.
func (p *Phones) Sealer(id string) (*relay.Sealer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, err := p.byIDLocked(id)
	if err != nil {
		return nil, err
	}
	return relay.NewSealer(d.KeyM2P, relay.M2P, relay.MacID(d.Origin), d.ID, d.SendN, func(upto uint64) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		d, err := p.byIDLocked(id)
		if err != nil {
			return err
		}
		if upto > d.SendN {
			d.SendN = upto
		}
		return p.saveLocked()
	})
}

// Opener opens the device's frames to the Mac. Each accepted counter is written to phones.json before the frame
// goes on, and a removed device's frames stop opening.
func (p *Phones) Opener(id string) (*relay.Opener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, err := p.byIDLocked(id)
	if err != nil {
		return nil, err
	}
	return relay.NewOpener(d.KeyP2M, relay.P2M, relay.MacID(d.Origin), d.ID, d.RecvN, func(n uint64) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		d, err := p.byIDLocked(id)
		if err != nil {
			return err
		}
		if n > d.RecvN {
			d.RecvN = n
		}
		return p.saveLocked()
	})
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

// Devices: the paired phones, newest first, without their tokens and keys.
func (p *Phones) Devices() []Device {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []Device{}
	for _, d := range p.data.Devices {
		c := *d
		c.Token, c.KeyP2M, c.KeyM2P = "", nil, nil
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
	return errors.New("device not connected")
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
			errs = append(errs, fmt.Errorf("%s: the notification subscription is no longer valid", d.Name))
		case res.StatusCode >= 300:
			errs = append(errs, fmt.Errorf("%s: the notification service responded %d", d.Name, res.StatusCode))
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
	return "Device"
}
