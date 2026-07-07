package controlapi

import (
	"sync"
	"time"
)

const (
	authFailMax    = 10              // failed attempts within the window before lockout
	authFailWindow = 1 * time.Minute // counting window
	authFailCool   = 1 * time.Minute // lockout duration once the threshold is hit
)

type ipFail struct {
	first time.Time // first failure in the current window
	count int       // failures since first
	until time.Time // lockout deadline; zero value = not locked
}

// ipLimiter is an in-process, per-IP failed-auth throttle. It is best-effort:
// behind a proxy all clients share the proxy's address, and state resets on
// restart. Its purpose is to slow online brute-forcing of bearer tokens
// (tokens are high-entropy, so this is defense-in-depth, not a primary control).
type ipLimiter struct {
	mu    sync.Mutex
	state map[string]*ipFail
}

func newIPLimiter() *ipLimiter {
	return &ipLimiter{state: make(map[string]*ipFail)}
}

// locked reports whether ip is currently in a lockout.
func (l *ipLimiter) locked(ip string) bool {
	if l == nil || ip == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[ip]
	if st == nil {
		return false
	}
	return time.Now().Before(st.until)
}

// note records an auth outcome for ip. Success clears the counter; failure
// accumulates and, once authFailMax is reached within the window, starts a
// lockout. Stale entries are lazily garbage-collected.
func (l *ipLimiter) note(ip string, success bool) {
	if l == nil || ip == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.gcLocked(now)
	if success {
		delete(l.state, ip)
		return
	}
	st := l.state[ip]
	if st == nil || now.Sub(st.first) >= authFailWindow {
		st = &ipFail{first: now}
		l.state[ip] = st
	}
	st.count++
	if st.count >= authFailMax && st.until.IsZero() {
		st.until = now.Add(authFailCool)
	}
}

func (l *ipLimiter) gcLocked(now time.Time) {
	for ip, st := range l.state {
		if now.Sub(st.first) >= authFailWindow && (st.until.IsZero() || now.After(st.until)) {
			delete(l.state, ip)
		}
	}
}
