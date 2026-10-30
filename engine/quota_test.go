package engine

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryQuotaStore struct {
	mu      sync.Mutex
	records []QuotaPersistentState
	saveErr error
	saves   int
}

func (store *memoryQuotaStore) LoadQuotaState() ([]QuotaPersistentState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return clonePersistentQuotaStates(store.records), nil
}

func (store *memoryQuotaStore) SaveQuotaState(records []QuotaPersistentState) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.saves++
	if store.saveErr != nil {
		return store.saveErr
	}
	store.records = clonePersistentQuotaStates(records)
	return nil
}

func (store *memoryQuotaStore) saveCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.saves
}

func clonePersistentQuotaStates(records []QuotaPersistentState) []QuotaPersistentState {
	result := make([]QuotaPersistentState, len(records))
	for index, record := range records {
		result[index] = QuotaPersistentState{RuleID: record.RuleID, Daily: clonePeriodMap(record.Daily), Monthly: clonePeriodMap(record.Monthly)}
	}
	return result
}

func testQuota() *TrafficQuota {
	return &TrafficQuota{Daily: 100, Monthly: 150, Timezone: "Asia/Shanghai", Action: "block"}
}

func TestQuotaLedgerEnforcesCombinedPeriodsAndTCPPartialFinalWrite(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	ledger, err := newQuotaLedger(nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	allowed, first, err := ledger.reserve("r1", testQuota(), 60, false, 10)
	if err != nil || allowed != 60 {
		t.Fatalf("first reserve = %d, %v", allowed, err)
	}
	first.finish(60)
	allowed, final, err := ledger.reserve("r1", testQuota(), 60, false, 10)
	if err != nil || allowed != 40 {
		t.Fatalf("final reserve = %d, %v", allowed, err)
	}
	final.finish(40)
	if _, _, err := ledger.reserve("r1", testQuota(), 1, false, 10); !errors.Is(err, ErrTrafficQuotaExceeded) {
		t.Fatalf("exhausted reserve error = %v", err)
	}
	snapshot := ledger.snapshot("r1", testQuota())
	if !snapshot.Blocked || snapshot.Daily.UsedBytes != 100 || snapshot.Monthly.UsedBytes != 100 {
		t.Fatalf("quota snapshot = %+v", snapshot)
	}
}

func TestQuotaLedgerUDPReservationIsAllOrNothing(t *testing.T) {
	ledger, _ := newQuotaLedger(nil, func() time.Time { return time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC) })
	quota := &TrafficQuota{Daily: 100, Timezone: "UTC", Action: "block"}
	allowed, reservation, err := ledger.reserve("udp", quota, 60, true, 10)
	if err != nil || allowed != 60 {
		t.Fatalf("first datagram = %d, %v", allowed, err)
	}
	reservation.finish(60)
	if allowed, _, err := ledger.reserve("udp", quota, 50, true, 10); allowed != 0 || !errors.Is(err, ErrTrafficQuotaExceeded) {
		t.Fatalf("oversized remaining datagram = %d, %v", allowed, err)
	}
	if used := ledger.snapshot("udp", quota).Daily.UsedBytes; used != 60 {
		t.Fatalf("used bytes = %d, want 60", used)
	}
}

func TestQuotaLedgerCalendarResetKeepsMonthlyUsage(t *testing.T) {
	now := time.Date(2026, 8, 2, 15, 59, 59, 0, time.UTC) // 23:59:59 Asia/Shanghai
	ledger, _ := newQuotaLedger(nil, func() time.Time { return now })
	quota := &TrafficQuota{Daily: 10, Monthly: 100, Timezone: "Asia/Shanghai", Action: "block"}
	allowed, reservation, err := ledger.reserve("calendar", quota, 10, true, 1)
	if err != nil || allowed != 10 {
		t.Fatal(err)
	}
	reservation.finish(10)
	if !ledger.snapshot("calendar", quota).Blocked {
		t.Fatal("quota should be blocked before midnight")
	}
	now = now.Add(2 * time.Second)
	snapshot := ledger.snapshot("calendar", quota)
	if snapshot.Blocked || snapshot.Daily.UsedBytes != 0 || snapshot.Monthly.UsedBytes != 10 || snapshot.Daily.Period != "2026-08-03" {
		t.Fatalf("post-reset snapshot = %+v", snapshot)
	}
}

func TestQuotaLedgerDurableCreditCannotBeRecoveredByRestart(t *testing.T) {
	store := new(memoryQuotaStore)
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	ledger, _ := newQuotaLedger(store, func() time.Time { return now })
	quota := &TrafficQuota{Daily: ByteSize(100 << 20), Timezone: "UTC", Action: "block"}
	allowed, reservation, err := ledger.reserve("durable", quota, 100, true, 1<<20)
	if err != nil || allowed != 100 {
		t.Fatal(err)
	}
	reservation.finish(100)
	ledger.waitIdle() // the watermark flush runs in the background
	restored, err := newQuotaLedger(store, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	used := restored.snapshot("durable", quota).Daily.UsedBytes
	if used < 100 || used > 1<<20 {
		t.Fatalf("restored conservative usage = %d", used)
	}
}

func TestQuotaLedgerPersistenceFailureFailsClosed(t *testing.T) {
	store := &memoryQuotaStore{saveErr: errors.New("disk full")}
	current := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	ledger, _ := newQuotaLedger(store, func() time.Time { return current })
	// The grant itself succeeds; its watermark flush fails afterwards. Bytes
	// already granted stay charged (they were forwarded).
	if allowed, _, err := ledger.reserve("failed", testQuota(), 1, true, 1); allowed != 1 || err != nil {
		t.Fatalf("reserve = %d, %v", allowed, err)
	}
	ledger.waitIdle() // let the background flush round fail and record flushErr
	if saves := store.saveCount(); saves != 1 {
		t.Fatalf("save attempts after failure = %d, want 1", saves)
	}
	// Once the failure is observed, later reserves fail closed until a save
	// succeeds, and the retry cooldown keeps a broken store from being
	// hammered once per packet.
	if allowed, _, err := ledger.reserve("failed", testQuota(), 1, true, 1); allowed != 0 || !errors.Is(err, ErrQuotaStatePersist) {
		t.Fatalf("gated reserve = %d, %v", allowed, err)
	}
	if allowed, _, err := ledger.reserve("failed", testQuota(), 1, true, 1); allowed != 0 || !errors.Is(err, ErrQuotaStatePersist) {
		t.Fatalf("still gated reserve = %d, %v", allowed, err)
	}
	if saves := store.saveCount(); saves != 1 {
		t.Fatalf("cooldown did not throttle save attempts: %d saves", saves)
	}
	// Disk recovers; the gate retries once the cooldown elapses and traffic
	// resumes.
	store.mu.Lock()
	store.saveErr = nil
	store.mu.Unlock()
	current = current.Add(quotaPersistRetryCooldown + time.Second)
	if allowed, _, err := ledger.reserve("failed", testQuota(), 1, true, 1); allowed != 1 || err != nil {
		t.Fatalf("recovered reserve = %d, %v", allowed, err)
	}
	if used := ledger.snapshot("failed", testQuota()).Daily.UsedBytes; used != 2 {
		t.Fatalf("charged bytes = %d", used)
	}
}

// TestQuotaLedgerSaveExactLiftsPersistGate pins that an exact save made after
// forwarding stopped clears a recorded persist failure, so the next reserve
// resumes immediately instead of waiting out the retry cooldown.
func TestQuotaLedgerSaveExactLiftsPersistGate(t *testing.T) {
	store := &memoryQuotaStore{saveErr: errors.New("disk full")}
	current := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	ledger, _ := newQuotaLedger(store, func() time.Time { return current })
	if allowed, _, err := ledger.reserve("exact", testQuota(), 1, true, 1); allowed != 1 || err != nil {
		t.Fatalf("reserve = %d, %v", allowed, err)
	}
	ledger.waitIdle()
	if allowed, _, err := ledger.reserve("exact", testQuota(), 1, true, 1); allowed != 0 || !errors.Is(err, ErrQuotaStatePersist) {
		t.Fatalf("gated reserve = %d, %v", allowed, err)
	}
	store.mu.Lock()
	store.saveErr = nil
	store.mu.Unlock()
	if err := ledger.SaveExact(); err != nil {
		t.Fatalf("SaveExact = %v", err)
	}
	if allowed, _, err := ledger.reserve("exact", testQuota(), 1, true, 1); allowed != 1 || err != nil {
		t.Fatalf("post-SaveExact reserve = %d, %v (gate should be lifted)", allowed, err)
	}
}

// TestQuotaLedgerPrunesOldPeriodKeys pins that quota-state.json cannot grow
// without bound: retention is calendar-based (active period plus the two
// before it, with a one-key transient), so keys older than the rollback window
// disappear from the persisted state.
func TestQuotaLedgerPrunesOldPeriodKeys(t *testing.T) {
	store := new(memoryQuotaStore)
	current := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ledger, _ := newQuotaLedger(store, func() time.Time { return current })
	quota := &TrafficQuota{Daily: 10, Monthly: 100, Timezone: "UTC", Action: "block"}
	reserve := func() {
		allowed, reservation, err := ledger.reserve("prune", quota, 10, true, 10)
		if err != nil || allowed != 10 {
			t.Fatalf("reserve at %s = %d, %v", current.Format(time.RFC3339), allowed, err)
		}
		reservation.finish(10)
	}
	for range 6 { // daily churn converges the daily map
		current = current.Add(24 * time.Hour)
		reserve()
	}
	for range 5 { // monthly churn converges the monthly map
		current = current.Add(32 * 24 * time.Hour)
		reserve()
	}
	ledger.waitIdle()
	records := func() []QuotaPersistentState {
		store.mu.Lock()
		defer store.mu.Unlock()
		return clonePersistentQuotaStates(store.records)
	}()
	var daily, monthly map[string]int64
	for _, record := range records {
		if record.RuleID == "prune" {
			daily, monthly = record.Daily, record.Monthly
		}
	}
	if len(daily) > quotaRetainedPastPeriods+2 {
		t.Fatalf("daily keys = %v, bound is %d", daily, quotaRetainedPastPeriods+2)
	}
	if len(monthly) > quotaRetainedPastPeriods+2 {
		t.Fatalf("monthly keys = %v, bound is %d", monthly, quotaRetainedPastPeriods+2)
	}
	if daily["2026-06-16"] != 10 || monthly["2026-06"] != 10 {
		t.Fatalf("active period usage missing after pruning: daily=%v monthly=%v", daily, monthly)
	}
	if daily["2026-01-02"] != 0 || monthly["2026-01"] != 0 {
		t.Fatalf("stale period keys survived pruning: daily=%v monthly=%v", daily, monthly)
	}
}

type blockingQuotaStore struct {
	mu      sync.Mutex
	records []QuotaPersistentState
	entered chan struct{}
	proceed chan struct{}
	saves   int
}

func (store *blockingQuotaStore) LoadQuotaState() ([]QuotaPersistentState, error) {
	return nil, nil
}

func (store *blockingQuotaStore) SaveQuotaState(records []QuotaPersistentState) error {
	store.mu.Lock()
	store.saves++
	store.mu.Unlock()
	store.entered <- struct{}{}
	<-store.proceed
	store.mu.Lock()
	defer store.mu.Unlock()
	store.records = clonePersistentQuotaStates(records)
	return nil
}

// TestQuotaLedgerFlushDoesNotBlockOtherRules pins the write-behind contract:
// while one rule's watermark save is parked inside the store (slow fsync),
// another rule's reserve must complete without waiting for it, and the parked
// flush must coalesce the other rule's bump into its drain round.
func TestQuotaLedgerFlushDoesNotBlockOtherRules(t *testing.T) {
	entered := make(chan struct{}, 16)
	proceed := make(chan struct{})
	store := &blockingQuotaStore{entered: entered, proceed: proceed}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ledger, _ := newQuotaLedger(store, func() time.Time { return now })
	quota := &TrafficQuota{Daily: 1 << 30, Timezone: "UTC", Action: "block"}

	reserveDone := make(chan int64, 2)
	reserve := func(ruleID string) {
		allowed, _, err := ledger.reserve(ruleID, quota, 64, true, 64)
		if err != nil {
			t.Errorf("reserve %s: %v", ruleID, err)
		}
		reserveDone <- allowed
	}

	go reserve("rule-a")
	<-entered // rule A's save is parked inside the store, off the data path

	done := make(chan int64, 1)
	go func() {
		allowed, _, err := ledger.reserve("rule-b", quota, 64, true, 64)
		if err != nil {
			t.Errorf("reserve rule-b: %v", err)
		}
		done <- allowed
	}()
	select {
	case allowed := <-done:
		if allowed != 64 {
			t.Fatalf("rule-b allowed = %d", allowed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rule-b reserve blocked behind rule-a's in-flight flush")
	}

	close(proceed)
	if allowed := <-reserveDone; allowed != 64 {
		t.Fatalf("rule-a allowed = %d", allowed)
	}
	ledger.waitIdle() // the drain round lands rule B's coalesced bump

	// Rule A's flush drained rule B's bump: both persisted in the parked
	// round plus one drain round.
	records := func() []QuotaPersistentState {
		store.mu.Lock()
		defer store.mu.Unlock()
		return clonePersistentQuotaStates(store.records)
	}()
	byRule := map[string]QuotaPersistentState{}
	for _, record := range records {
		byRule[record.RuleID] = record
	}
	if byRule["rule-a"].Daily["2026-01-01"] == 0 || byRule["rule-b"].Daily["2026-01-01"] == 0 {
		t.Fatalf("persisted records missing durable marks: %+v", records)
	}
	store.mu.Lock()
	saves := store.saves
	store.mu.Unlock()
	if saves != 2 {
		t.Fatalf("save rounds = %d, want 2 (parked round + coalesced drain)", saves)
	}
}

func TestQuotaLedgerConcurrentReservationsNeverExceedLimit(t *testing.T) {
	ledger, _ := newQuotaLedger(nil, time.Now)
	quota := &TrafficQuota{Daily: 100, Timezone: "UTC", Action: "block"}
	var wg sync.WaitGroup
	var accepted int64
	var acceptedMu sync.Mutex
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			allowed, reservation, err := ledger.reserve("concurrent", quota, 20, true, 1)
			if err == nil {
				reservation.finish(allowed)
				acceptedMu.Lock()
				accepted += allowed
				acceptedMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 100 || ledger.snapshot("concurrent", quota).Daily.UsedBytes != 100 {
		t.Fatalf("accepted = %d, snapshot = %+v", accepted, ledger.snapshot("concurrent", quota))
	}
}

func TestCopyWithTrafficQuotaWritesExactRemainingBytes(t *testing.T) {
	ledger, _ := newQuotaLedger(nil, time.Now)
	rule := Rule{RuleID: "copy", TrafficQuota: &TrafficQuota{Daily: 10, Timezone: "UTC", Action: "block"}}
	controller := newRuleTrafficController(rule, ledger)
	var output bytes.Buffer
	var rejected int64
	written, err := copyWithTrafficOptions(context.Background(), &output, bytes.NewBufferString("0123456789abcdefghij"), 0, copyOptions{}, controller, trafficUpload, func(n int64) { rejected += n }, nil)
	if !errors.Is(err, ErrTrafficQuotaExceeded) || written != 10 || output.String() != "0123456789" || rejected != 10 {
		t.Fatalf("copy = written %d, output %q, rejected %d, error %v", written, output.String(), rejected, err)
	}
}

func TestSharedRateLimiterAggregatesConcurrentWaiters(t *testing.T) {
	limiter := newSharedRateLimiter(256<<10, 64<<10)
	if err := limiter.wait(context.Background(), 64<<10); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := limiter.wait(context.Background(), 32<<10); err != nil {
				t.Errorf("wait: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	if elapsed < 180*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("aggregate wait elapsed %v, want about 250ms", elapsed)
	}
}

func TestRuleTrafficControllerAggregatesConcurrentTCPCopies(t *testing.T) {
	const rate = int64(256 << 10)
	controller := newRuleTrafficController(Rule{
		RuleID: "aggregate-tcp", Bandwidth: &BandwidthLimit{Rate: ByteRate(rate)},
	}, nil)
	if controller.upload == nil || controller.download == nil || controller.upload == controller.download {
		t.Fatal("symmetric rule rate must use independent upload and download buckets")
	}
	burst := automaticTrafficBurst(rate)
	if err := controller.wait(context.Background(), trafficUpload, int(burst)); err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte{'x'}, 32<<10)
	started := time.Now()
	var wg sync.WaitGroup
	var written int64
	var writtenMu sync.Mutex
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := copyWithTrafficOptions(
				context.Background(), &bytes.Buffer{}, bytes.NewReader(payload), 0,
				copyOptions{}, controller, trafficUpload, nil, nil,
			)
			if err != nil {
				t.Errorf("copy: %v", err)
				return
			}
			writtenMu.Lock()
			written += n
			writtenMu.Unlock()
		}()
	}
	wg.Wait()
	if written != 64<<10 {
		t.Fatalf("written = %d, want %d", written, 64<<10)
	}
	if elapsed := time.Since(started); elapsed < 180*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("two TCP copies completed in %v, want aggregate delay around 250ms", elapsed)
	}
}
