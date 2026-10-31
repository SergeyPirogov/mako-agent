package dispatch

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// A tracker that cannot be searched (no internet, the VPN dropped, the service is down, the token expired) is
// said on the board in words, with the tracker and its host, instead of a raw «dial tcp: lookup …».

// outage: why the search of t failed, in words: "GitLab · git.example.com: немає з'єднання — перевірте
// інтернет чи VPN".
func outage(t trackers.Tracker, err error) string {
	host := TrackerHost(t)
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}
	who := t.Name()
	if host != "" {
		who += " · " + host
	}
	return who + ": " + outageReason(err)
}

func outageReason(err error) string {
	var he *trackers.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status == 401 || he.Status == 403:
			return "доступ не дано — перевірте токен у налаштуваннях"
		case he.Status == 429:
			return "забагато запитів, сервіс просить зачекати"
		case he.Status >= 500:
			return fmt.Sprintf("сервіс не працює (%d), спробую пізніше", he.Status)
		}
		return fmt.Sprintf("сервіс відповів помилкою %d", he.Status)
	}
	var dns *net.DNSError
	var cert x509.UnknownAuthorityError
	var host x509.HostnameError
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "пошук перервано"
	case errors.As(err, &dns):
		return "сервер не знайдено — перевірте інтернет чи VPN"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "сервіс не приймає з'єднань — він лежить чи потрібен VPN"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN):
		return "мережа недоступна — перевірте інтернет чи VPN"
	case errors.Is(err, syscall.ECONNRESET):
		return "з'єднання обірвалось — перевірте інтернет чи VPN"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "сервіс не відповідає — перевірте інтернет чи VPN"
	case errors.As(err, &cert), errors.As(err, &host), strings.Contains(err.Error(), "x509:"):
		return "сертифікат сервера не прийнято"
	}
	return "пошук не вдався"
}

// TrackerHost: the tracker's web host, "" when it cannot be told.
func TrackerHost(t trackers.Tracker) string {
	if ch, ok := t.(trackers.CodeHost); ok {
		return ch.Host()
	}
	if c := t.Config(); c != nil && c.BaseURL != "" {
		if u, err := url.Parse(c.BaseURL); err == nil {
			return u.Hostname()
		}
	}
	return ""
}
