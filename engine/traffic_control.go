package engine

import (
	"context"
	"sync"
	"time"
)

type trafficDirection uint8

const (
	trafficUpload trafficDirection = iota
	trafficDownload
	minimumTrafficBurst = int64(64 << 10)
	maximumTrafficBurst = int64(4 << 20)
)

type ruleTrafficController struct {
	ruleID       string
	quota        *TrafficQuota
	ledger       *QuotaLedger
	upload       *sharedRateLimiter
	download     *sharedRateLimiter
	durableChunk int64
}

func newRuleTrafficController(rule Rule, ledger *QuotaLedger) *ruleTrafficController {
	if ledger == nil {
		ledger, _ = newQuotaLedger(nil, time.Now)
	}
	controller := &ruleTrafficController{ruleID: rule.RuleID, quota: cloneTrafficQuota(rule.TrafficQuota), ledger: ledger}
	if rule.Bandwidth != nil {
		upload := rule.Bandwidth.UploadBytesPerSecond()
		download := rule.Bandwidth.DownloadBytesPerSecond()
		controller.upload = newSharedRateLimiter(upload, automaticTrafficBurst(upload))
		controller.download = newSharedRateLimiter(download, automaticTrafficBurst(download))
		controller.durableChunk = durableQuotaChunk(upload, download, rule.TrafficQuota)
	} else {
		controller.durableChunk = durableQuotaChunk(0, 0, rule.TrafficQuota)
	}
	return controller
}

func cloneTrafficQuota(quota *TrafficQuota) *TrafficQuota {
	if quota == nil {
		return nil
	}
	copy := *quota
	return &copy
}

func automaticTrafficBurst(rate int64) int64 {
	if rate <= 0 {
		return 0
	}
	return min(max(rate/10, minimumTrafficBurst), maximumTrafficBurst)
}

func durableQuotaChunk(upload, download int64, quota *TrafficQuota) int64 {
	chunk := max(upload+download, int64(8<<20))
	chunk = min(max(chunk, int64(1<<20)), int64(64<<20))
	var smallest int64
	if quota != nil {
		for _, limit := range []int64{int64(quota.Daily), int64(quota.Monthly)} {
			if limit > 0 && (smallest == 0 || limit < smallest) {
				smallest = limit
			}
		}
	}
	if smallest > 0 {
		chunk = min(chunk, max(int64(64<<10), smallest/100))
	}
	return chunk
}

func (controller *ruleTrafficController) wait(ctx context.Context, direction trafficDirection, bytes int) error {
	if controller == nil || bytes <= 0 {
		return nil
	}
	limiter := controller.upload
	if direction == trafficDownload {
		limiter = controller.download
	}
	return limiter.wait(ctx, bytes)
}

func (controller *ruleTrafficController) rateLimited(direction trafficDirection) bool {
	if controller == nil {
		return false
	}
	if direction == trafficDownload {
		return controller.download != nil
	}
	return controller.upload != nil
}

func (controller *ruleTrafficController) reserve(bytes int64, allOrNothing bool) (int64, *quotaReservation, error) {
	if controller == nil || controller.ledger == nil {
		return bytes, nil, nil
	}
	return controller.ledger.reserve(controller.ruleID, controller.quota, bytes, allOrNothing, controller.durableChunk)
}

func (controller *ruleTrafficController) quotaBlocked() bool {
	return controller != nil && controller.ledger != nil && controller.ledger.blocked(controller.ruleID, controller.quota)
}

type rateWaiter struct {
	turn chan struct{}
}

// sharedRateLimiter is a FIFO token bucket safe for every connection and UDP
// session belonging to one rule direction.
type sharedRateLimiter struct {
	mu     sync.Mutex
	rate   int64
	burst  int64
	tokens float64
	last   time.Time
	queue  []*rateWaiter
}

func newSharedRateLimiter(rate, burst int64) *sharedRateLimiter {
	if rate <= 0 {
		return nil
	}
	burst = max(burst, minimumTrafficBurst)
	return &sharedRateLimiter{rate: rate, burst: burst, tokens: float64(burst), last: time.Now()}
}

func (limiter *sharedRateLimiter) wait(ctx context.Context, bytes int) error {
	if limiter == nil || bytes <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waiter := &rateWaiter{turn: make(chan struct{})}
	limiter.mu.Lock()
	limiter.queue = append(limiter.queue, waiter)
	if len(limiter.queue) == 1 {
		close(waiter.turn)
	}
	limiter.mu.Unlock()

	select {
	case <-ctx.Done():
		limiter.cancel(waiter)
		return ctx.Err()
	case <-waiter.turn:
	}
	for {
		if err := ctx.Err(); err != nil {
			limiter.cancel(waiter)
			return err
		}
		limiter.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(limiter.last).Seconds()
		if elapsed > 0 {
			limiter.tokens = min(float64(limiter.burst), limiter.tokens+elapsed*float64(limiter.rate))
			limiter.last = now
		}
		if limiter.tokens >= float64(bytes) {
			limiter.tokens -= float64(bytes)
			limiter.popHeadLocked(waiter)
			limiter.mu.Unlock()
			return nil
		}
		missing := float64(bytes) - limiter.tokens
		wait := time.Duration(missing / float64(limiter.rate) * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		limiter.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			limiter.cancel(waiter)
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (limiter *sharedRateLimiter) cancel(waiter *rateWaiter) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	for index, current := range limiter.queue {
		if current != waiter {
			continue
		}
		wasHead := index == 0
		limiter.queue = append(limiter.queue[:index], limiter.queue[index+1:]...)
		if wasHead && len(limiter.queue) > 0 {
			close(limiter.queue[0].turn)
		}
		return
	}
}

func (limiter *sharedRateLimiter) popHeadLocked(waiter *rateWaiter) {
	if len(limiter.queue) == 0 || limiter.queue[0] != waiter {
		return
	}
	limiter.queue = limiter.queue[1:]
	if len(limiter.queue) > 0 {
		close(limiter.queue[0].turn)
	}
}
