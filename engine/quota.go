package engine

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

var (
	ErrTrafficQuotaExceeded = errors.New("traffic quota exceeded")
	ErrQuotaStatePersist    = errors.New("persist traffic quota state")
)

// QuotaPersistentState is the durable high-water mark for one rule. Period
// maps retain prior keys so a clock rollback cannot recreate an empty period.
type QuotaPersistentState struct {
	RuleID  string           `json:"rule_id"`
	Daily   map[string]int64 `json:"daily,omitempty"`
	Monthly map[string]int64 `json:"monthly,omitempty"`
}

// QuotaStateStore provides strict persistence for enforced traffic quotas.
//
// Durability contract with QuotaLedger's write-behind flush: a LoadQuotaState
// error stays fatal at startup, but a SaveQuotaState error does not roll back
// the traffic granted in that round. The failed save gates every further
// reserve (fail-closed going forward) until one succeeds, and a crash may
// under-count usage by up to one durable chunk per rule and period.
type QuotaStateStore interface {
	LoadQuotaState() ([]QuotaPersistentState, error)
	SaveQuotaState([]QuotaPersistentState) error
}

type quotaRuleUsage struct {
	dailyUsed      map[string]int64
	dailyDurable   map[string]int64
	monthlyUsed    map[string]int64
	monthlyDurable map[string]int64
}

type QuotaLedger struct {
	mu        sync.Mutex
	store     QuotaStateStore
	now       func() time.Time
	states    map[string]*quotaRuleUsage
	locations map[string]*time.Location

	// Write-behind persistence: durable watermark bumps are applied under mu
	// but persisted by a background single-flight flush goroutine, so a slow
	// store (fsync) never blocks any rule's chunk accounting. Every field
	// below is guarded by mu; flushing is the single-flight guard for the
	// in-progress save round and flushCond wakes waiters when it completes.
	// flushFailAt throttles retry attempts after a failed save so a broken
	// store cannot be hammered once per packet or connection.
	flushPending bool
	flushErr     error
	flushing     bool
	flushFailAt  time.Time
	flushCond    *sync.Cond
}

func NewQuotaLedger(store QuotaStateStore) (*QuotaLedger, error) {
	return newQuotaLedger(store, time.Now)
}

func newQuotaLedger(store QuotaStateStore, now func() time.Time) (*QuotaLedger, error) {
	if now == nil {
		now = time.Now
	}
	ledger := &QuotaLedger{
		store:     store,
		now:       now,
		states:    make(map[string]*quotaRuleUsage),
		locations: make(map[string]*time.Location),
	}
	ledger.flushCond = sync.NewCond(&ledger.mu)
	if store == nil {
		return ledger, nil
	}
	records, err := store.LoadQuotaState()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if record.RuleID == "" {
			return nil, fmt.Errorf("quota state contains an empty rule_id")
		}
		if _, duplicate := ledger.states[record.RuleID]; duplicate {
			return nil, fmt.Errorf("quota state contains duplicate rule_id %q", record.RuleID)
		}
		state := newQuotaRuleUsage()
		if err := restoreQuotaPeriods(state.dailyUsed, state.dailyDurable, record.Daily); err != nil {
			return nil, fmt.Errorf("quota state for %s daily: %w", record.RuleID, err)
		}
		if err := restoreQuotaPeriods(state.monthlyUsed, state.monthlyDurable, record.Monthly); err != nil {
			return nil, fmt.Errorf("quota state for %s monthly: %w", record.RuleID, err)
		}
		ledger.states[record.RuleID] = state
	}
	return ledger, nil
}

func newQuotaRuleUsage() *quotaRuleUsage {
	return &quotaRuleUsage{
		dailyUsed:      make(map[string]int64),
		dailyDurable:   make(map[string]int64),
		monthlyUsed:    make(map[string]int64),
		monthlyDurable: make(map[string]int64),
	}
}

func restoreQuotaPeriods(used, durable, source map[string]int64) error {
	for period, value := range source {
		if period == "" || value < 0 {
			return fmt.Errorf("invalid period %q or byte count %d", period, value)
		}
		used[period] = value
		durable[period] = value
	}
	return nil
}

type quotaReservation struct {
	ledger        *QuotaLedger
	ruleID        string
	dailyPeriod   string
	monthlyPeriod string
	bytes         int64
	done          bool
}

func (reservation *quotaReservation) finish(written int64) {
	if reservation == nil || reservation.ledger == nil || reservation.done {
		return
	}
	if written < 0 {
		written = 0
	}
	if written > reservation.bytes {
		written = reservation.bytes
	}
	refund := reservation.bytes - written
	reservation.done = true
	if refund == 0 {
		return
	}
	ledger := reservation.ledger
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	state := ledger.states[reservation.ruleID]
	if state == nil {
		return
	}
	refundQuotaPeriod(state.dailyUsed, reservation.dailyPeriod, refund)
	refundQuotaPeriod(state.monthlyUsed, reservation.monthlyPeriod, refund)
}

func refundQuotaPeriod(periods map[string]int64, period string, amount int64) {
	if period == "" || amount <= 0 {
		return
	}
	if current := periods[period]; current > amount {
		periods[period] = current - amount
	} else {
		periods[period] = 0
	}
}

// quotaPersistRetryCooldown throttles save attempts while the gate is up, so a
// broken store is retried once per cooldown instead of once per packet.
const quotaPersistRetryCooldown = time.Second

// reserve grants quota bytes for one forwarding chunk. The durable watermark
// is bumped under mu, but the store save runs outside mu (write-behind), so an
// fsync in the store never holds up other rules' reserves. Durability
// contract: a crash may under-count usage by up to one durable chunk per rule
// and period; a failed save gates further reserves (fail-closed) until one
// succeeds.
func (ledger *QuotaLedger) reserve(ruleID string, quota *TrafficQuota, requested int64, allOrNothing bool, durableChunk int64) (int64, *quotaReservation, error) {
	if requested <= 0 || quota == nil || (quota.Daily <= 0 && quota.Monthly <= 0) {
		return requested, nil, nil
	}

	ledger.mu.Lock()
	if ledger.flushErr != nil {
		persistErr := ledger.flushErr
		if ledger.now().Sub(ledger.flushFailAt) < quotaPersistRetryCooldown {
			// Still inside the retry cooldown: fail closed without another
			// doomed save attempt on the forwarding data path.
			ledger.mu.Unlock()
			return 0, nil, fmt.Errorf("%w: %v", ErrQuotaStatePersist, persistErr)
		}
		ledger.flushPending = true
		retry := false
		if !ledger.flushing {
			ledger.flushing = true
			retry = true
		}
		ledger.mu.Unlock()
		if !retry {
			// Another goroutine is already saving; fail closed and let the
			// next reserve observe its outcome.
			return 0, nil, fmt.Errorf("%w: %v", ErrQuotaStatePersist, persistErr)
		}
		if err := ledger.runFlush(); err != nil {
			return 0, nil, fmt.Errorf("%w: %v", ErrQuotaStatePersist, err)
		}
		ledger.mu.Lock()
	}

	periods, err := ledger.periodsLocked(quota)
	if err != nil {
		ledger.mu.Unlock()
		return 0, nil, err
	}
	state := ledger.states[ruleID]
	if state == nil {
		state = newQuotaRuleUsage()
		ledger.states[ruleID] = state
	}
	pruneQuotaPeriods(state, periods)
	allowed := requested
	if quota.Daily > 0 {
		allowed = min(allowed, max(0, int64(quota.Daily)-state.dailyUsed[periods.daily]))
	}
	if quota.Monthly > 0 {
		allowed = min(allowed, max(0, int64(quota.Monthly)-state.monthlyUsed[periods.monthly]))
	}
	if allowed <= 0 || (allOrNothing && allowed < requested) {
		ledger.mu.Unlock()
		return 0, nil, ErrTrafficQuotaExceeded
	}
	if durableChunk <= 0 {
		durableChunk = 8 << 20
	}
	changed := false
	if quota.Daily > 0 {
		// growDurablePeriod must run for monthly even when the daily call
		// already returned true, hence grow(...) || changed and not the
		// reverse; reordering would silently drop monthly durable marks.
		changed = growDurablePeriod(state.dailyDurable, state.dailyUsed, periods.daily, int64(quota.Daily), allowed, durableChunk) || changed
	}
	if quota.Monthly > 0 {
		changed = growDurablePeriod(state.monthlyDurable, state.monthlyUsed, periods.monthly, int64(quota.Monthly), allowed, durableChunk) || changed
	}
	flush := false
	if changed && ledger.store != nil {
		ledger.flushPending = true
		if !ledger.flushing {
			ledger.flushing = true
			flush = true
		}
	}
	if quota.Daily > 0 {
		state.dailyUsed[periods.daily] += allowed
	}
	if quota.Monthly > 0 {
		state.monthlyUsed[periods.monthly] += allowed
	}
	reservation := &quotaReservation{
		ledger:        ledger,
		ruleID:        ruleID,
		dailyPeriod:   activePeriod(quota.Daily > 0, periods.daily),
		monthlyPeriod: activePeriod(quota.Monthly > 0, periods.monthly),
		bytes:         allowed,
	}
	ledger.mu.Unlock()

	// The grant is complete; hand the watermark save to a background
	// single-flight flusher so no forwarding goroutine waits on store I/O.
	if flush {
		go ledger.runFlush()
	}
	return allowed, reservation, nil
}

// runFlush persists pending watermark bumps and must be entered only after
// winning the flushing single-flight guard. Each round snapshots under mu and
// saves outside it, then keeps draining until no bump is pending, so requests
// raised during a save coalesce into the next round. A failed save is recorded
// in flushErr (gating later reserves, throttled by quotaPersistRetryCooldown);
// a drained round clears it.
func (ledger *QuotaLedger) runFlush() error {
	for {
		ledger.mu.Lock()
		if !ledger.flushPending {
			ledger.flushErr = nil
			// Clear the single-flight guard under mu: a goroutine that just
			// set flushPending then either sees flushing==false and takes
			// over, or is covered by this drain loop's next iteration.
			ledger.flushing = false
			ledger.flushCond.Broadcast()
			ledger.mu.Unlock()
			return nil
		}
		snapshot := ledger.persistentStateLocked()
		ledger.flushPending = false
		ledger.mu.Unlock()
		if err := ledger.store.SaveQuotaState(snapshot); err != nil {
			ledger.mu.Lock()
			ledger.flushErr = err
			ledger.flushFailAt = ledger.now()
			ledger.flushing = false
			ledger.flushCond.Broadcast()
			ledger.mu.Unlock()
			// Attempting is throttled by the retry cooldown, so this logs at
			// most once per cooldown while a store stays broken.
			log.Printf("[vmflow] persist quota state failed, gating new traffic until a save succeeds: %v", err)
			return err
		}
	}
}

// quotaRetainedPastPeriods is how many calendar periods before the active one
// keep their usage keys. Older keys are pruned so quota-state.json cannot grow
// without bound, while a recent clock rollback still cannot recreate an empty
// period (which would re-open allowance).
const quotaRetainedPastPeriods = 2

// pruneQuotaPeriods drops usage and durable keys older than the retained
// window. Period keys are fixed-width UTC-format calendar strings, so cutoff
// comparison is a plain lexicographic test.
func pruneQuotaPeriods(state *quotaRuleUsage, periods quotaPeriods) {
	if len(state.dailyUsed) > quotaRetainedPastPeriods+1 {
		dailyCutoff := periods.dailyReset.AddDate(0, 0, -(quotaRetainedPastPeriods + 1)).Format("2006-01-02")
		prunePeriodMap(state.dailyUsed, dailyCutoff)
		prunePeriodMap(state.dailyDurable, dailyCutoff)
	}
	if len(state.monthlyUsed) > quotaRetainedPastPeriods+1 {
		monthlyCutoff := periods.monthlyReset.AddDate(0, -(quotaRetainedPastPeriods + 1), 0).Format("2006-01")
		prunePeriodMap(state.monthlyUsed, monthlyCutoff)
		prunePeriodMap(state.monthlyDurable, monthlyCutoff)
	}
}

func prunePeriodMap(values map[string]int64, cutoff string) {
	for period := range values {
		if period < cutoff {
			delete(values, period)
		}
	}
}

// waitIdle blocks until no watermark bump is pending and no flush round is in
// flight. It makes asynchronous persistence observable for callers that need
// the durable state settled (tests and shutdown paths).
func (ledger *QuotaLedger) waitIdle() {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for ledger.flushPending || ledger.flushing {
		ledger.flushCond.Wait()
	}
}

func activePeriod(enabled bool, period string) string {
	if enabled {
		return period
	}
	return ""
}

func growDurablePeriod(durable, used map[string]int64, period string, limit, allowed, chunk int64) bool {
	required := used[period] + allowed
	if durable[period] >= required {
		return false
	}
	target := max(required, durable[period]+chunk)
	durable[period] = min(limit, target)
	return true
}

type quotaPeriods struct {
	daily, monthly           string
	dailyReset, monthlyReset time.Time
}

func (ledger *QuotaLedger) periodsLocked(quota *TrafficQuota) (quotaPeriods, error) {
	timezone := quota.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	location := ledger.locations[timezone]
	if location == nil {
		var err error
		location, err = time.LoadLocation(timezone)
		if err != nil {
			return quotaPeriods{}, err
		}
		ledger.locations[timezone] = location
	}
	now := ledger.now().In(location)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	return quotaPeriods{
		daily:        dayStart.Format("2006-01-02"),
		monthly:      monthStart.Format("2006-01"),
		dailyReset:   dayStart.AddDate(0, 0, 1),
		monthlyReset: monthStart.AddDate(0, 1, 0),
	}, nil
}

// TrafficQuotaSnapshot describes the active calendar-period budgets for a rule.
type TrafficQuotaSnapshot struct {
	Daily   *QuotaPeriodSnapshot `json:"daily,omitempty"`
	Monthly *QuotaPeriodSnapshot `json:"monthly,omitempty"`
	Blocked bool                 `json:"blocked"`
}

type QuotaPeriodSnapshot struct {
	Period         string `json:"period"`
	UsedBytes      int64  `json:"used_bytes"`
	LimitBytes     int64  `json:"limit_bytes"`
	RemainingBytes int64  `json:"remaining_bytes"`
	ResetTime      int64  `json:"reset_time"`
	Exhausted      bool   `json:"exhausted"`
}

func (ledger *QuotaLedger) snapshot(ruleID string, quota *TrafficQuota) *TrafficQuotaSnapshot {
	if ledger == nil || quota == nil || (quota.Daily <= 0 && quota.Monthly <= 0) {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	periods, err := ledger.periodsLocked(quota)
	if err != nil {
		return &TrafficQuotaSnapshot{Blocked: true}
	}
	state := ledger.states[ruleID]
	result := &TrafficQuotaSnapshot{}
	if quota.Daily > 0 {
		used := periodUsage(state, true, periods.daily)
		result.Daily = quotaPeriodSnapshot(periods.daily, used, int64(quota.Daily), periods.dailyReset)
		result.Blocked = result.Blocked || result.Daily.Exhausted
	}
	if quota.Monthly > 0 {
		used := periodUsage(state, false, periods.monthly)
		result.Monthly = quotaPeriodSnapshot(periods.monthly, used, int64(quota.Monthly), periods.monthlyReset)
		result.Blocked = result.Blocked || result.Monthly.Exhausted
	}
	return result
}

func periodUsage(state *quotaRuleUsage, daily bool, period string) int64 {
	if state == nil {
		return 0
	}
	if daily {
		return state.dailyUsed[period]
	}
	return state.monthlyUsed[period]
}

func quotaPeriodSnapshot(period string, used, limit int64, reset time.Time) *QuotaPeriodSnapshot {
	remaining := max(0, limit-used)
	return &QuotaPeriodSnapshot{
		Period: period, UsedBytes: used, LimitBytes: limit, RemainingBytes: remaining,
		ResetTime: reset.Unix(), Exhausted: used >= limit,
	}
}

func (ledger *QuotaLedger) blocked(ruleID string, quota *TrafficQuota) bool {
	snapshot := ledger.snapshot(ruleID, quota)
	return snapshot != nil && snapshot.Blocked
}

// SaveExact must be called only after all forwarding runners have stopped. It
// replaces conservative durable credits with actual successful byte counts.
func (ledger *QuotaLedger) SaveExact() error {
	if ledger == nil || ledger.store == nil {
		return nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	// Wait out any in-flight write-behind round so its conservative snapshot
	// cannot overwrite the exact state written below.
	for ledger.flushing {
		ledger.flushCond.Wait()
	}
	ledger.flushPending = false // this exact save supersedes any pending bump
	for _, state := range ledger.states {
		state.dailyDurable = clonePeriodMap(state.dailyUsed)
		state.monthlyDurable = clonePeriodMap(state.monthlyUsed)
	}
	if err := ledger.store.SaveQuotaState(ledger.persistentStateLocked()); err != nil {
		return fmt.Errorf("%w: %v", ErrQuotaStatePersist, err)
	}
	// The exact save succeeded with no runner left to fail later: lift the
	// gate so reserves resume without a redundant retry round.
	ledger.flushErr = nil
	return nil
}

func clonePeriodMap(source map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (ledger *QuotaLedger) persistentStateLocked() []QuotaPersistentState {
	ids := make([]string, 0, len(ledger.states))
	for ruleID := range ledger.states {
		ids = append(ids, ruleID)
	}
	sort.Strings(ids)
	result := make([]QuotaPersistentState, 0, len(ids))
	for _, ruleID := range ids {
		state := ledger.states[ruleID]
		result = append(result, QuotaPersistentState{
			RuleID: ruleID, Daily: clonePeriodMap(state.dailyDurable), Monthly: clonePeriodMap(state.monthlyDurable),
		})
	}
	return result
}
