package engine

import "testing"

func TestBoundRuleStatsUpdatesSnapshot(t *testing.T) {
	collector := NewCollector()
	stats := collector.bindRule(" rule-1 ")
	stats.addUpload(10)
	stats.addDownload(20)
	stats.incConns()
	stats.incUDPSessionRejected()
	stats.incUDPPacketsDropped()

	snapshot := collector.Snapshot("rule-1")
	if snapshot.UploadBytes != 10 || snapshot.DownloadBytes != 20 || snapshot.Conns != 1 {
		t.Fatalf("unexpected traffic snapshot: %+v", snapshot)
	}
	if snapshot.UDPSessionRejected != 1 || snapshot.UDPPacketsDropped != 1 {
		t.Fatalf("unexpected UDP protection counters: %+v", snapshot)
	}
}

func TestSnapshotDoesNotRecreateRemovedRule(t *testing.T) {
	collector := NewCollector()
	collector.EnsureRule("removed")
	collector.RemoveRule("removed")

	snapshot := collector.Snapshot("removed")
	if snapshot.RuleID != "removed" || snapshot.UploadBytes != 0 {
		t.Fatalf("unexpected missing rule snapshot: %+v", snapshot)
	}
	if snapshots := collector.SnapshotAll(); len(snapshots) != 0 {
		t.Fatalf("Snapshot recreated removed counter: %+v", snapshots)
	}
}

func TestNonPositiveTrafficDoesNotCreateRule(t *testing.T) {
	collector := NewCollector()
	collector.AddUpload("zero", 0)
	collector.AddDownload("negative", -1)
	if snapshots := collector.SnapshotAll(); len(snapshots) != 0 {
		t.Fatalf("non-positive traffic created counters: %+v", snapshots)
	}
}

func TestRuleProtocolMetadataPersistsUntilRemoval(t *testing.T) {
	collector := NewCollector()
	collector.EnsureRuleProtocol("rule", ProtocolUDP)
	if got := collector.RuleProtocols()["rule"]; got != ProtocolUDP {
		t.Fatalf("protocol = %q, want udp", got)
	}
	collector.RemoveRule("rule")
	if _, ok := collector.RuleProtocols()["rule"]; ok {
		t.Fatal("removed rule protocol metadata was retained")
	}
}

func TestNilCollectorStatsAreNoop(t *testing.T) {
	var collector *Collector
	collector.AddUpload("rule", 1)
	collector.AddDownload("rule", 1)
	collector.IncConns("rule")
	collector.DecConns("rule")
	collector.SetConns("rule", 0)
	collector.IncUDPSessionRejected("rule")
	collector.IncUDPPacketsDropped("rule")
}

func TestBoundRuleStatsDoesNotAllocate(t *testing.T) {
	stats := NewCollector().bindRule("rule")
	allocs := testing.AllocsPerRun(1000, func() {
		stats.addUpload(32 * 1024)
	})
	if allocs != 0 {
		t.Fatalf("bound stats allocated %.2f objects per update", allocs)
	}
}

func BenchmarkBoundRuleStatsParallel(b *testing.B) {
	stats := NewCollector().bindRule("rule")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			stats.addUpload(32 * 1024)
		}
	})
}
