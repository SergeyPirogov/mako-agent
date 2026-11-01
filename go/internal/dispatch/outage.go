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
	if login := t.Config().Assignee; badAssignee(err) && login != "" {
		return who + fmt.Sprintf(": користувача «%s» немає — виправте «Лише призначені на» в налаштуваннях", login)
	}
	return who + ": " + outageReason(err)
}

func outageReason(err error) string {
	reason, _ := classify(err)
	return reason
}

// classify: a failure of a tracker in words, and whether it is the network or the service (no internet or VPN, a
// dropped connection, a 429 or 5xx) rather than Mako or its settings. One place for both, so a new cause is not
// added to the words and forgotten in the verdict.
func classify(err error) (reason string, outside bool) {
	var he *trackers.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Status == 401 || he.Status == 403:
			return "доступ не дано — перевірте токен у налаштуваннях", false
		case he.Status == 429:
			return "забагато запитів, сервіс просить зачекати", true
		case he.Status >= 500:
			return fmt.Sprintf("сервіс не працює (%d), спробую пізніше", he.Status), true
		}
		return fmt.Sprintf("сервіс відповів помилкою %d", he.Status), false
	}
	var dns *net.DNSError
	var cert x509.UnknownAuthorityError
	var host x509.HostnameError
	var ue *url.Error
	var oe *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return "пошук перервано", true
	case errors.As(err, &dns):
		return "сервер не знайдено — перевірте інтернет чи VPN", true
	case errors.Is(err, syscall.ECONNREFUSED):
		return "сервіс не приймає з'єднань — він лежить чи потрібен VPN", true
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN),
		errors.Is(err, syscall.EADDRNOTAVAIL), strings.Contains(err.Error(), "can't assign requested address"):
		return "мережа недоступна — перевірте інтернет чи VPN", true
	case errors.Is(err, syscall.ECONNRESET):
		return "з'єднання обірвалось — перевірте інтернет чи VPN", true
	// Only the network's own timeouts: a deadline Mako set itself (an agent, a reviewer) is not the network.
	case errors.As(err, &ue) && ue.Timeout(), errors.As(err, &oe) && oe.Timeout():
		return "сервіс не відповідає — перевірте інтернет чи VPN", true
	case errors.As(err, &cert), errors.As(err, &host), strings.Contains(err.Error(), "x509:"):
		return "сертифікат сервера не прийнято", false
	case errors.Is(err, context.DeadlineExceeded):
		return "сервіс не відповідає — перевірте інтернет чи VPN", false
	}
	return "пошук не вдався", false
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

// badAssignee: the tracker refused the search for its assignee (GitHub: 422 on an unknown login).
func badAssignee(err error) bool {
	var he *trackers.HTTPError
	return errors.As(err, &he) && (he.Status == 422 || he.Status == 400) && strings.Contains(he.Body, `"assignee"`)
}

// transientErr: the failure comes from the network or the service, not from Mako, so it is only journaled.
func transientErr(err error) bool {
	_, outside := classify(err)
	return outside
}
