// login_limiter.go 管理登录限速：同一 IP 每分钟最多 10 次尝试。
// 管理密钥本身是强随机串；限速是纵深防御，也避免登录端点被拿来做爆破/放大。
package admin

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	loginMaxPerMinute = 10
	loginWindow       = time.Minute
)

type loginLimiter struct {
	mu          sync.Mutex
	hits        map[string][]time.Time
	lastCleanup time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{hits: map[string][]time.Time{}}
}

func (l *loginLimiter) allow(ip string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-loginWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= loginMaxPerMinute {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	if now.Sub(l.lastCleanup) > loginWindow {
		l.lastCleanup = now
		for key, times := range l.hits {
			keep := times[:0]
			for _, t := range times {
				if t.After(cutoff) {
					keep = append(keep, t)
				}
			}
			if len(keep) == 0 {
				delete(l.hits, key)
			} else {
				l.hits[key] = keep
			}
		}
	}
	return true
}

// loginClientIP resolves the client address behind Cloudflare / cloudflared.
func loginClientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
