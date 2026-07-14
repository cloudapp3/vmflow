package engine

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ruleCounter struct {
	uploadTotal        atomic.Int64
	downloadTotal      atomic.Int64
	conns              atomic.Int64
	udpSessionRejected atomic.Int64
	udpPacketsDropped  atomic.Int64
	updatedAtUnix      atomic.Int64
}

// boundRuleStats is a rule-local handle used by forwarding runners. Binding
// once keeps rule ID normalization and the collector map lock out of the data
// path while preserving Collector's public rule-ID based API.
type boundRuleStats struct {
	counter *ruleCounter
}

type Collector struct {
	mu        sync.RWMutex
	counters  map[string]*ruleCounter
	protocols map[string]Protocol
}

func NewCollector() *Collector {
	return &Collector{
		counters:  make(map[string]*ruleCounter),
		protocols: make(map[string]Protocol),
	}
}

func (collector *Collector) EnsureRule(ruleID string) {
	if collector == nil {
		return
	}
	collector.getCounter(stringsTrim(ruleID))
}

func (collector *Collector) EnsureRuleProtocol(ruleID string, protocol Protocol) {
	if collector == nil {
		return
	}
	ruleID = stringsTrim(ruleID)
	collector.mu.Lock()
	if collector.counters[ruleID] == nil {
		counter := &ruleCounter{}
		counter.updatedAtUnix.Store(time.Now().Unix())
		collector.counters[ruleID] = counter
	}
	collector.protocols[ruleID] = standardizeProtocol(protocol)
	collector.mu.Unlock()
}

// RemoveRule discards a rule's counters and protocol metadata. Forwarding for
// the rule must be stopped first; Manager.RemoveRule enforces that ordering.
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
	delete(collector.protocols, ruleID)
	collector.mu.Unlock()
}

func (collector *Collector) RuleProtocols() map[string]Protocol {
	if collector == nil {
		return nil
	}
	collector.mu.RLock()
	protocols := make(map[string]Protocol, len(collector.protocols))
	for ruleID, protocol := range collector.protocols {
		protocols[ruleID] = protocol
	}
	collector.mu.RUnlock()
	return protocols
}

func (collector *Collector) AddUpload(ruleID string, n int64) {
	if collector == nil || n <= 0 {
		return
	}
	collector.bindRule(ruleID).addUpload(n)
}

func (collector *Collector) AddDownload(ruleID string, n int64) {
	if collector == nil || n <= 0 {
		return
	}
	collector.bindRule(ruleID).addDownload(n)
}

func (collector *Collector) IncConns(ruleID string) {
	collector.bindRule(ruleID).incConns()
}

func (collector *Collector) DecConns(ruleID string) {
	collector.bindRule(ruleID).decConns()
}

func (collector *Collector) SetConns(ruleID string, value int64) {
	collector.bindRule(ruleID).setConns(value)
}

func (collector *Collector) IncUDPSessionRejected(ruleID string) {
	collector.bindRule(ruleID).incUDPSessionRejected()
}

func (collector *Collector) IncUDPPacketsDropped(ruleID string) {
	collector.bindRule(ruleID).incUDPPacketsDropped()
}

func (collector *Collector) Snapshot(ruleID string) TrafficSnapshot {
	if collector == nil {
		return TrafficSnapshot{RuleID: stringsTrim(ruleID)}
	}
	ruleID = stringsTrim(ruleID)
	counter := collector.lookupCounter(ruleID)
	return snapshotCounter(ruleID, counter)
}

func (collector *Collector) SnapshotAll() []TrafficSnapshot {
	if collector == nil {
		return nil
	}
	collector.mu.RLock()
	type item struct {
		ruleID  string
		counter *ruleCounter
	}
	items := make([]item, 0, len(collector.counters))
	for ruleID, counter := range collector.counters {
		items = append(items, item{ruleID: ruleID, counter: counter})
	}
	collector.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ruleID < items[j].ruleID })

	result := make([]TrafficSnapshot, 0, len(items))
	for _, item := range items {
		result = append(result, snapshotCounter(item.ruleID, item.counter))
	}
	return result
}

func (collector *Collector) bindRule(ruleID string) boundRuleStats {
	if collector == nil {
		return boundRuleStats{}
	}
	return boundRuleStats{counter: collector.getCounter(stringsTrim(ruleID))}
}

func (collector *Collector) lookupCounter(ruleID string) *ruleCounter {
	if collector == nil {
		return nil
	}
	collector.mu.RLock()
	counter := collector.counters[ruleID]
	collector.mu.RUnlock()
	return counter
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

func (stats boundRuleStats) addUpload(n int64) {
	if stats.counter == nil || n <= 0 {
		return
	}
	stats.counter.uploadTotal.Add(n)
	stats.touch()
}

func (stats boundRuleStats) addDownload(n int64) {
	if stats.counter == nil || n <= 0 {
		return
	}
	stats.counter.downloadTotal.Add(n)
	stats.touch()
}

func (stats boundRuleStats) incConns() {
	if stats.counter == nil {
		return
	}
	stats.counter.conns.Add(1)
	stats.touch()
}

func (stats boundRuleStats) decConns() {
	if stats.counter == nil {
		return
	}
	stats.counter.conns.Add(-1)
	stats.touch()
}

func (stats boundRuleStats) setConns(value int64) {
	if stats.counter == nil {
		return
	}
	stats.counter.conns.Store(value)
	stats.touch()
}

func (stats boundRuleStats) incUDPSessionRejected() {
	if stats.counter == nil {
		return
	}
	stats.counter.udpSessionRejected.Add(1)
	stats.touch()
}

func (stats boundRuleStats) incUDPPacketsDropped() {
	if stats.counter == nil {
		return
	}
	stats.counter.udpPacketsDropped.Add(1)
	stats.touch()
}

func (stats boundRuleStats) touch() {
	stats.counter.updatedAtUnix.Store(time.Now().Unix())
}

func snapshotCounter(ruleID string, counter *ruleCounter) TrafficSnapshot {
	if counter == nil {
		return TrafficSnapshot{RuleID: ruleID}
	}
	return TrafficSnapshot{
		RuleID:             ruleID,
		UploadBytes:        counter.uploadTotal.Load(),
		DownloadBytes:      counter.downloadTotal.Load(),
		Conns:              counter.conns.Load(),
		UDPSessionRejected: counter.udpSessionRejected.Load(),
		UDPPacketsDropped:  counter.udpPacketsDropped.Load(),
		UpdatedTime:        counter.updatedAtUnix.Load(),
	}
}
