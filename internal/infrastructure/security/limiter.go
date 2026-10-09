package security

import (
	"strings"
	"sync"
	"time"

	"github.com/XuLaiQ/all2api/internal/ports"
)

const LoginWindow = 10 * time.Minute

type LoginLimiter struct {
	maxFails int
	mu       sync.Mutex
	attempts map[string][]int64
}

var _ ports.LoginLimiter = (*LoginLimiter)(nil)

func NewLoginLimiter(maxFails int) *LoginLimiter {
	return &LoginLimiter{maxFails: maxFails, attempts: make(map[string][]int64)}
}

func (l *LoginLimiter) Begin(ip, username string, now time.Time) int {
	nowUnix := now.Unix()
	keys := []string{"ip:" + ip, "user:" + strings.ToLower(username)}
	l.mu.Lock()
	defer l.mu.Unlock()
	byKey := make(map[string][]int64, len(keys))
	blockedUntil := int64(0)
	for _, key := range keys {
		fresh := make([]int64, 0, len(l.attempts[key]))
		for _, at := range l.attempts[key] {
			if nowUnix-at < int64(LoginWindow/time.Second) {
				fresh = append(fresh, at)
			}
		}
		byKey[key] = fresh
		if len(fresh) >= l.maxFails {
			candidate := fresh[0] + int64(LoginWindow/time.Second)
			if candidate > blockedUntil {
				blockedUntil = candidate
			}
		}
	}
	if wait := blockedUntil - nowUnix; wait > 0 {
		return int(wait)
	}
	for key, attempts := range byKey {
		l.attempts[key] = append(attempts, nowUnix)
	}
	if len(l.attempts) > 4096 {
		for key, attempts := range l.attempts {
			if len(attempts) == 0 || nowUnix-attempts[len(attempts)-1] >= int64(LoginWindow/time.Second) {
				delete(l.attempts, key)
			}
		}
	}
	return 0
}

func (l *LoginLimiter) Clear(ip, username string) {
	l.mu.Lock()
	delete(l.attempts, "ip:"+ip)
	delete(l.attempts, "user:"+strings.ToLower(username))
	l.mu.Unlock()
}
