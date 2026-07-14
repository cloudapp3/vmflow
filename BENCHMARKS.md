# Forwarding benchmarks

These benchmarks provide a reproducible relative baseline for vmflow's active
TCP and UDP data paths. They are not a physical-NIC throughput guarantee.

## Environment

- Linux/amd64
- Go 1.25.12
- 6 vCPU AMD EPYC Milan
- loopback traffic
- benchmark date: 2026-07-12

Run the full benchmark suite with:

```bash
make bench
```

The focused data-path command used for the results below is:

```bash
go test ./engine -run '^$' \
  -bench '^Benchmark(TCPForwardingPersistent|TCPForwardingShortConnection|UDPForwardingRoundTrip|BoundRuleStatsParallel)$' \
  -benchmem -benchtime=2s -count=3
```

## Current observation

The ranges below are observations from repeated runs on this host, not CI
thresholds. Scheduler and host load can move short-latency results by several
percent.

| Benchmark | Direct | Forwarded |
|---|---:|---:|
| Persistent TCP echo, aggregate | 3.39-3.62 GB/s | 1.42-1.58 GB/s |
| TCP connect + 1-byte echo | 139-163 us/op | 297-349 us/op |
| TCP short-connection allocation | 1.67 KB/op | 4.29-4.37 KB/op |
| UDP 64-byte sequential RTT | 16.2-17.2 us/op | 32.6-34.6 us/op |
| UDP 1400-byte sequential RTT | 16.4-17.3 us/op | 33.5-35.0 us/op |
| UDP per-operation allocation | 52 B, 2 allocs | 52-53 B, 2 allocs |

The bound traffic counter benchmark is 50-83 ns/op with zero allocations on
this host. The 10,000-rule control-plane benchmarks are approximately 1.1 ms
for the metrics protocol index and 11.6-12.1 ms for precheck.

UDP capacity is bounded separately from packet throughput. Each active UDP
session owns one socket, one receive goroutine, and one pooled 64 KiB receive
buffer. The daemon defaults to 256 active UDP sessions globally and accepts an
explicit maximum of 4096. Check process memory and open-file limits before
raising the default; the latency benchmarks above use one active session and
are not capacity measurements.

## Interpretation

The persistent TCP benchmark is a sequential loopback echo and reports upload
plus download bytes. Use it to compare revisions on the same host, not to infer
10 GbE line rate. Real release qualification should also include physical-NIC
tests, concurrent connections, small-packet PPS, loss measurements, and a
long-running soak test.

The generic Go copy path deliberately retains live counters, rate limiting,
connection-level idle handling, and bounded writes. A materially faster Linux
path would require a separately tested `splice` implementation while keeping
those semantics intact.
