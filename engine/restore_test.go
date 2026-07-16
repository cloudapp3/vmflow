package engine

import "testing"

func TestCollectorRestoreSeedsCounters(t *testing.T) {
	collector := NewCollector()
	collector.Restore([]TrafficSnapshot{
		{RuleID: "r1", UploadBytes: 100, DownloadBytes: 200, SourceIPDenied: 2, UDPSessionRejected: 3, UDPPacketsDropped: 5},
		{RuleID: "r2", UploadBytes: 50, DownloadBytes: 7},
	})
	snap := collector.Snapshot("r1")
	if snap.UploadBytes != 100 || snap.DownloadBytes != 200 || snap.SourceIPDenied != 2 || snap.UDPSessionRejected != 3 || snap.UDPPacketsDropped != 5 {
		t.Fatalf("r1 not restored: %+v", snap)
	}
	// Conns is a live connection count; it must not be restored (a fresh daemon
	// has zero connections, and restoring the old value would be misleading).
	if snap.Conns != 0 {
		t.Fatalf("conns must not be restored: %d", snap.Conns)
	}
	if collector.Snapshot("r2").UploadBytes != 50 || collector.Snapshot("r2").DownloadBytes != 7 {
		t.Fatalf("r2 not restored")
	}
}

func TestCollectorRestoreNilSafe(t *testing.T) {
	var collector *Collector
	collector.Restore([]TrafficSnapshot{{RuleID: "r1", UploadBytes: 1}})
	collector.Restore(nil)
}
