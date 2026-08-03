package engine

import (
	"errors"
	"fmt"
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

func (ledger *QuotaLedger) reserve(ruleID string, quota *TrafficQuota, requested int64, allOrNothing bool, durableChunk int64) (int64, *quotaReservation, error) {
	if requested <= 0 || quota == nil || (quota.Daily <= 0 && quota.Monthly <= 0) {
		return requested, nil, nil
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()

	periods, err := ledger.periodsLocked(quota)
	if err != nil {
		return 0, nil, err
	}
	state := ledger.states[ruleID]
	if state == nil {
		state = newQuotaRuleUsage()
		ledger.states[ruleID] = state
	}
	allowed := requested
	if quota.Daily > 0 {
		allowed = min(allowed, max(0, int64(quota.Daily)-state.dailyUsed[periods.daily]))
	}
	if quota.Monthly > 0 {
		allowed = min(allowed, max(0, int64(quota.Monthly)-state.monthlyUsed[periods.monthly]))
	}
	if allowed <= 0 || (allOrNothing && allowed < requested) {
		return 0, nil, ErrTrafficQuotaExceeded
	}
	if durableChunk <= 0 {
		durableChunk = 8 << 20
	}

	rollback := captureDurablePeriods(state, periods)
	changed := false
	if quota.Daily > 0 {
		changed = growDurablePeriod(state.dailyDurable, state.dailyUsed, periods.daily, int64(quota.Daily), allowed, durableChunk) || changed
	}
	if quota.Monthly > 0 {
		changed = growDurablePeriod(state.monthlyDurable, state.monthlyUsed, periods.monthly, int64(quota.Monthly), allowed, durableChunk) || changed
	}
	if changed && ledger.store != nil {
		if err := ledger.store.SaveQuotaState(ledger.persistentStateLocked()); err != nil {
			rollback.restore(state)
			return 0, nil, fmt.Errorf("%w: %v", ErrQuotaStatePersist, err)
		}
	}
	if quota.Daily > 0 {
		state.dailyUsed[periods.daily] += allowed
	}
	if quota.Monthly > 0 {
		state.monthlyUsed[periods.monthly] += allowed
	}
	return allowed, &quotaReservation{
		ledger:        ledger,
		ruleID:        ruleID,
		dailyPeriod:   activePeriod(quota.Daily > 0, periods.daily),
		monthlyPeriod: activePeriod(quota.Monthly > 0, periods.monthly),
		bytes:         allowed,
	}, nil
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

type durableRollback struct {
	dailyValue, monthlyValue   int64
	dailyFound, monthlyFound   bool
	dailyPeriod, monthlyPeriod string
}

func captureDurablePeriods(state *quotaRuleUsage, periods quotaPeriods) durableRollback {
	daily, dailyFound := state.dailyDurable[periods.daily]
	monthly, monthlyFound := state.monthlyDurable[periods.monthly]
	return durableRollback{
		dailyValue: daily, monthlyValue: monthly,
		dailyFound: dailyFound, monthlyFound: monthlyFound,
		dailyPeriod: periods.daily, monthlyPeriod: periods.monthly,
	}
}

func (rollback durableRollback) restore(state *quotaRuleUsage) {
	restoreMapValue(state.dailyDurable, rollback.dailyPeriod, rollback.dailyValue, rollback.dailyFound)
	restoreMapValue(state.monthlyDurable, rollback.monthlyPeriod, rollback.monthlyValue, rollback.monthlyFound)
}

func restoreMapValue(values map[string]int64, key string, value int64, found bool) {
	if found {
		values[key] = value
	} else {
		delete(values, key)
	}
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
	for _, state := range ledger.states {
		state.dailyDurable = clonePeriodMap(state.dailyUsed)
		state.monthlyDurable = clonePeriodMap(state.monthlyUsed)
	}
	if err := ledger.store.SaveQuotaState(ledger.persistentStateLocked()); err != nil {
		return fmt.Errorf("%w: %v", ErrQuotaStatePersist, err)
	}
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
