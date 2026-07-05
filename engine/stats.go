package engine

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ruleCounter struct {
	uploadTotal   atomic.Int64
	downloadTotal atomic.Int64
	conns         atomic.Int64
	updatedAtUnix atomic.Int64
}

type Collector struct {
	mu       sync.RWMutex
	counters map[string]*ruleCounter
}

func NewCollector() *Collector {
	return &Collector{counters: make(map[string]*ruleCounter)}
}

func (collector *Collector) EnsureRule(ruleID string) {
	if collector == nil {
		return
	}
	collector.getCounter(stringsTrim(ruleID))
}

func (collector *Collector) RemoveRule(ruleID string) {
	if collector == nil {
		return
	}
	ruleID = stringsTrim(ruleID)
	if ruleID == "" {
		return
	}
	collector.mu.Lock()
	delete(collector.counters, ruleID)
	collector.mu.Unlock()
}

func (collector *Collector) AddUpload(ruleID string, n int64) {
	if collector == nil || n <= 0 {
		return
	}
	counter := collector.getCounter(stringsTrim(ruleID))
	counter.uploadTotal.Add(n)
	counter.updatedAtUnix.Store(time.Now().Unix())
}

func (collector *Collector) AddDownload(ruleID string, n int64) {
	if collector == nil || n <= 0 {
		return
	}
	counter := collector.getCounter(stringsTrim(ruleID))
	counter.downloadTotal.Add(n)
	counter.updatedAtUnix.Store(time.Now().Unix())
}

func (collector *Collector) IncConns(ruleID string) {
	if collector == nil {
		return
	}
	counter := collector.getCounter(stringsTrim(ruleID))
	counter.conns.Add(1)
	counter.updatedAtUnix.Store(time.Now().Unix())
}

func (collector *Collector) DecConns(ruleID string) {
	if collector == nil {
		return
	}
	counter := collector.getCounter(stringsTrim(ruleID))
	counter.conns.Add(-1)
	counter.updatedAtUnix.Store(time.Now().Unix())
}

func (collector *Collector) SetConns(ruleID string, value int64) {
	if collector == nil {
		return
	}
	counter := collector.getCounter(stringsTrim(ruleID))
	counter.conns.Store(value)
	counter.updatedAtUnix.Store(time.Now().Unix())
}

func (collector *Collector) Snapshot(ruleID string) TrafficSnapshot {
	if collector == nil {
		return TrafficSnapshot{RuleID: stringsTrim(ruleID)}
	}
	counter := collector.getCounter(stringsTrim(ruleID))
	return TrafficSnapshot{
		RuleID:        stringsTrim(ruleID),
		UploadBytes:   counter.uploadTotal.Load(),
		DownloadBytes: counter.downloadTotal.Load(),
		Conns:         counter.conns.Load(),
		UpdatedTime:   counter.updatedAtUnix.Load(),
	}
}

func (collector *Collector) SnapshotAll() []TrafficSnapshot {
	if collector == nil {
		return nil
	}
	collector.mu.RLock()
	ids := make([]string, 0, len(collector.counters))
	for ruleID := range collector.counters {
		ids = append(ids, ruleID)
	}
	collector.mu.RUnlock()
	sort.Strings(ids)

	result := make([]TrafficSnapshot, 0, len(ids))
	for _, ruleID := range ids {
		result = append(result, collector.Snapshot(ruleID))
	}
	return result
}

func (collector *Collector) getCounter(ruleID string) *ruleCounter {
	collector.mu.RLock()
	counter, ok := collector.counters[ruleID]
	collector.mu.RUnlock()
	if ok {
		return counter
	}

	collector.mu.Lock()
	defer collector.mu.Unlock()
	counter, ok = collector.counters[ruleID]
	if ok {
		return counter
	}
	counter = &ruleCounter{}
	counter.updatedAtUnix.Store(time.Now().Unix())
	collector.counters[ruleID] = counter
	return counter
}

func stringsTrim(value string) string {
	return strings.TrimSpace(value)
}
