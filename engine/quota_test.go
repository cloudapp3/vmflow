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
}

func (store *memoryQuotaStore) LoadQuotaState() ([]QuotaPersistentState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return clonePersistentQuotaStates(store.records), nil
}

func (store *memoryQuotaStore) SaveQuotaState(records []QuotaPersistentState) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.saveErr != nil {
		return store.saveErr
	}
	store.records = clonePersistentQuotaStates(records)
	return nil
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
	ledger, _ := newQuotaLedger(store, time.Now)
	if allowed, _, err := ledger.reserve("failed", testQuota(), 1, true, 1); allowed != 0 || !errors.Is(err, ErrQuotaStatePersist) {
		t.Fatalf("reserve = %d, %v", allowed, err)
	}
	if used := ledger.snapshot("failed", testQuota()).Daily.UsedBytes; used != 0 {
		t.Fatalf("failed persistence charged %d bytes", used)
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
