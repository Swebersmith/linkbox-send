package app

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func uuid() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func (a *App) tokenHash(token string) string {
	h := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	h.Write([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}
func validName(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= 255 && s != "." && s != ".." && !strings.ContainsAny(s, "/\\") && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) })
}
func (a *App) clientIP(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	if a.cfg.TrustProxy {
		ip := net.ParseIP(host)
		for _, block := range a.proxies {
			if block.Contains(ip) {
				if p := net.ParseIP(r.Header.Get("X-Real-IP")); p != nil {
					return p.String()
				}
				break
			}
		}
	}
	return host
}

type rateEntry struct {
	count int
	until time.Time
}
type limiter struct {
	mu    sync.Mutex
	items map[string]rateEntry
}

func (l *limiter) allow(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.items == nil {
		l.items = map[string]rateEntry{}
	}
	if len(l.items) >= 10000 {
		for k, v := range l.items {
			if now.After(v.until) {
				delete(l.items, k)
			}
		}
		if len(l.items) >= 10000 {
			return false
		}
	}
	entry := l.items[key]
	if now.After(entry.until) {
		entry = rateEntry{until: now.Add(window)}
	}
	if entry.count >= limit {
		return false
	}
	entry.count++
	l.items[key] = entry
	return true
}
