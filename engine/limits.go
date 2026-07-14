package engine

import "sync"

const (
	// DefaultUDPGlobalMaxSessions caps the total number of active UDP sessions
	// across all rules owned by one Manager. A zero ManagerOptions value uses
	// this limit rather than disabling the guard.
	DefaultUDPGlobalMaxSessions = 256

	// MaxUDPGlobalMaxSessions bounds configuration mistakes. A generic UDP
	// proxy consumes a socket and receive loop per active client address, so a
	// larger value is not a practical safe default for this implementation.
	MaxUDPGlobalMaxSessions = 4_096
)

// ManagerOptions controls manager-wide forwarding resource limits.
type ManagerOptions struct {
	// UDPMaxSessions uses DefaultUDPGlobalMaxSessions when non-positive and is
	// clamped to MaxUDPGlobalMaxSessions when larger than the supported bound.
	UDPMaxSessions int
}

type udpSessionBudget struct {
	mu     sync.Mutex
	limit  int
	active int
}

func newUDPSessionBudget(limit int) *udpSessionBudget {
	return &udpSessionBudget{limit: normalizeUDPGlobalMaxSessions(limit)}
}

func normalizeUDPGlobalMaxSessions(limit int) int {
	if limit <= 0 {
		return DefaultUDPGlobalMaxSessions
	}
	if limit > MaxUDPGlobalMaxSessions {
		return MaxUDPGlobalMaxSessions
	}
	return limit
}

func (budget *udpSessionBudget) tryAcquire() bool {
	if budget == nil {
		return true
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.active >= budget.limit {
		return false
	}
	budget.active++
	return true
}

func (budget *udpSessionBudget) release() {
	if budget == nil {
		return
	}
	budget.mu.Lock()
	if budget.active > 0 {
		budget.active--
	}
	budget.mu.Unlock()
}

func (budget *udpSessionBudget) setLimit(limit int) {
	if budget == nil {
		return
	}
	budget.mu.Lock()
	budget.limit = normalizeUDPGlobalMaxSessions(limit)
	budget.mu.Unlock()
}

func (budget *udpSessionBudget) snapshot() (limit, active int) {
	if budget == nil {
		return DefaultUDPGlobalMaxSessions, 0
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.limit, budget.active
}
