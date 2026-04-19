# quic-go Optimization Memo

Date: 2026-04-19
Branch: `sid`

## Goal

This memo records likely optimization directions for the local `quic-go` fork.

Primary objectives:

- stable
- fast
- avoid long-running memory / timer / goroutine overhead

## Current Context

Current branch:

- `sid`

Recent visible branch head:

- `2083199a` `update mod name`

Recent upstream-adjacent work near HEAD already includes:

- Retry AEAD lazy creation
- MTU probe path cleanup
- a connection ID rotation memory leak fix
- a Transport dial / close race fix

That means this review should focus less on already-addressed regressions and more on:

- transport hot paths
- packet I/O overhead
- timer / cleanup strategies
- queue / buffer behavior under scale

## High-Priority Areas

### 1. `sys_conn_oob.go`

Why it matters:

- this is the most sensitive UDP packet I/O hot path
- it handles:
  - ECN
  - packet info
  - batch reads
  - GSO writes
- any avoidable allocation or unnecessary control-message rebuilding here scales with packet rate

Current observations:

- read-side message / OOB buffers are preallocated once per connection, which is good
- write-side OOB composition still rebuilds ancillary data on every send
- `WritePacket` appends UDP_SEGMENT and ECN control messages for each packet send
- packet receive timestamps still call `time.Now()` per packet

Optimization ideas:

- reduce per-send ancillary buffer construction cost
- consider small reusable OOB builders for common GSO / ECN combinations
- audit whether timestamp acquisition granularity can be relaxed or batched without correctness loss

### 2. `packet_handler_map.go`

Why it matters:

- global map on the connection ID path
- central lifecycle structure for active / retired / closed handlers
- high churn here means timer and goroutine overhead can grow quickly

Current observations:

- `Retire` uses `time.AfterFunc` per retired connection ID
- `ReplaceWithClosed` also uses `time.AfterFunc` for delayed cleanup
- debug logging mode launches a long-lived usage ticker goroutine

Optimization ideas:

- replace per-retirement timers with a shared cleanup scheduler / min-heap / wheel
- avoid one goroutine per debug usage logger if better lifecycle coupling is possible
- inspect whether closed / retired handler cleanup can be amortized

### 3. `datagram_queue.go`

Why it matters:

- DATAGRAM traffic can be bursty and allocation-heavy
- this queue directly affects QUIC DATAGRAM workloads and any higher-level feature built on top of them

Current observations:

- every received DATAGRAM frame copies payload into a new `[]byte`
- receive queue is bounded, which is good
- send queue already uses an allocation-free ringbuffer for frame pointers

Optimization ideas:

- investigate whether a pooled byte-slice strategy is worthwhile for received datagrams
- verify whether current copy behavior is required for all callers or only for safety / ownership separation
- measure whether high-DATAGRAM workloads spend significant time in payload copying

### 4. `send_queue.go`

Why it matters:

- packet sending is a core throughput path
- this queue mediates packet pacing / write readiness / producer backpressure

Current observations:

- current implementation uses a buffered channel of fixed size
- `len(channel)` is used for WouldBlock / availability signaling
- close semantics are clear and simple

Optimization ideas:

- benchmark whether a ringbuffer + explicit condition signaling outperforms the current channel approach under high PPS
- verify whether `available` signaling causes unnecessary wakeups in busy send scenarios

Note:

- this area is not obviously broken; optimization should be benchmark-driven

### 5. `buffer_pool.go`

Why it matters:

- buffer reuse directly affects packet path allocation pressure
- this is foundational to both receive and send paths

Current observations:

- object pooling exists for normal and large packet buffers
- `packetBuffer` reference counting is manual but simple
- no obvious leak pattern is visible from static inspection

Optimization ideas:

- add focused profiling around large-buffer retention under coalesced packet workloads
- confirm that large-buffer reuse does not cause disproportionate memory retention after bursts
- consider lightweight observability in benchmarks rather than production counters

## Medium-Priority Areas

### 6. `streams_map*` and stream lifecycle

Why it matters:

- stream-heavy workloads can create lock and queue pressure
- stream lifecycle is correctness-sensitive and easy to regress

Current observations:

- no obvious immediate bug from static reading
- this area is large and likely needs benchmark-guided work rather than speculative changes

Optimization ideas:

- inspect stream accept/open contention under many concurrent streams
- review reset / delete / queueControlFrame interactions for unnecessary work amplification

### 7. `metrics/pool.go`

Why it matters:

- minor, but this is already an allocation-avoidance helper
- useful as a signal that metrics paths care about per-call allocations

Current observations:

- simple and bounded
- not a current problem area

Conclusion:

- keep as is unless profiling points here

## Memory / Lifecycle Assessment

Current static judgment:

- no new obvious unbounded-memory bug stands out immediately
- the biggest risk is not classic leaking, but:
  - too many timers
  - too much ancillary/OOB reconstruction
  - payload copies on hot paths
  - churn under high packet / connection rates

Most likely “slow growth” suspects from this fork alone:

- retired / closed handler cleanup strategy in `packet_handler_map.go`
- large-buffer retention after bursts
- datagram payload copy pressure

## Suggested Optimization Order

1. `sys_conn_oob.go`
2. `packet_handler_map.go`
3. `datagram_queue.go`
4. `send_queue.go`
5. stream lifecycle / contention review

## Recommended Next Step

If continuing from this memo, start with:

- `sys_conn_oob.go`

Questions to answer:

- how much allocation comes from ancillary/OOB write-side construction?
- is the current GSO / ECN path doing avoidable per-packet work?
- can we reduce timer pressure in `packet_handler_map.go` without complicating correctness?

## Progress Log

### 2026-04-19 - Round 1: send-side OOB composition

Status:

- implemented
- verification partially blocked by missing Go toolchain in this environment

Files touched:

- `send_conn.go`
- `send_conn_oob.go`
- `send_conn_no_oob.go`
- `sys_conn_oob.go`
- `sys_conn_helper_linux.go`
- `send_conn_test.go`

What changed:

- moved `sendConn.Write` onto the existing `writePacket` helper so the Linux first-`sendmsg` retry path is now exercised on the main send path
- added cached no-GSO ECN OOB payloads in `send_conn` for IPv4 and IPv6 so steady-state ECN sends can reuse immutable ancillary data instead of rebuilding it every packet
- added a stack-backed OOB preparation path for GSO / ECN combinations in `send_conn`, so combined control messages are assembled before calling `rawConn.WritePacket`
- introduced `appendControlMessageSpace` and switched ECN / UDP_SEGMENT helpers to reuse reserved OOB capacity when available, instead of always growing via a fresh zero-slice append
- updated `send_conn` tests to assert against the fully composed OOB payloads now passed into `rawConn.WritePacket`

Expected effect:

- less repeated control-message work on the steady-state send path
- lower small-object churn when the caller already reserved ancillary buffer capacity
- clearer separation between "prepare OOB once" in `send_conn` and "write packet" in `rawConn`

Verification:

- `git diff --check` passed on 2026-04-19
- `go test` could not be run on 2026-04-19 because `go` was not installed in `PATH` in this environment

Follow-up still worth doing:

- when a Go toolchain is available, run focused verification for `send_conn` / `sys_conn_oob` first, then the broader package tests
- benchmark the cached no-GSO ECN path versus the previous append-on-every-send path
- evaluate whether one-off direct `rawConn.WritePacket(..., info.OOB(), ...)` callers are hot enough to justify their own packet-info OOB caching layer
- after this write-path pass, move to `packet_handler_map.go` timer amortization
