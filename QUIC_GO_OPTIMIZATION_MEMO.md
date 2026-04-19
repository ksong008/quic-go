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

### 2026-04-19 - Round 2: packet handler cleanup scheduling

Status:

- implemented
- verification partially blocked by missing Go toolchain in this environment

Files touched:

- `packet_handler_map.go`
- `packet_handler_map_test.go`

What changed:

- replaced per-call `time.AfterFunc` cleanup scheduling in `Retire` and `ReplaceWithClosed` with a shared cleanup queue
- added one internal cleanup loop per `packetHandlerMap`, driven by a single reusable timer and a coalesced wakeup channel
- kept the external behavior the same: retired IDs and closed-handler IDs are still removed only after `deleteRetiredConnsAfter`
- copied `ReplaceWithClosed` connection ID slices before enqueueing cleanup, so delayed removal doesn't depend on caller-owned backing arrays
- updated tests to close the map via `t.Cleanup`, preventing test-only background goroutine leaks now that the map owns a cleanup loop
- added focused tests for:
  - multiple retired connection IDs pending cleanup at once
  - `ReplaceWithClosed` removing multiple connection IDs together

Why this shape:

- deliberately chose a simple shared timer loop over a heap / timer wheel to keep the change small and easy to reason about
- this introduces one goroutine per `packetHandlerMap`, but removes the previous unbounded `AfterFunc` timer churn on connection-ID retirement paths
- for the current "stability first" rule, that tradeoff is preferable to a more aggressive scheduling structure

Expected effect:

- fewer timer allocations and callback goroutines when many connection IDs are retired over time
- more predictable cleanup behavior under high connection-ID churn
- lower lifecycle overhead without changing retirement semantics

Verification:

- `git diff --check` passed on 2026-04-19 after this round
- `go test` could not be run on 2026-04-19 because `go` was not installed in `PATH` in this environment

Follow-up still worth doing:

- when a Go toolchain is available, run focused tests for `packet_handler_map` and broader transport / connection tests
- benchmark timer allocation and goroutine counts before and after this change under synthetic connection-ID churn
- if profiling still points here, consider whether the shared cleanup queue needs ordering or batching refinements

### 2026-04-19 - Round 3: queued packet-info OOB caching

Status:

- implemented
- verification partially blocked by missing Go toolchain in this environment

Files touched:

- `packet_handler_map.go`
- `server.go`
- `transport.go`
- `packet_info_oob_cache_test.go`

What changed:

- cached `packetInfoOOB` when enqueueing delayed close packets from `packet_handler_map`, instead of rebuilding it later in the transport send loop
- introduced lightweight queued packet wrappers in `server.go` so Version Negotiation / Retry / INVALID_TOKEN / CONNECTION_REFUSED responses reuse the OOB bytes prepared when the packet was queued
- introduced a lightweight queued stateless reset wrapper in `transport.go` so the stateless reset send path also reuses packet-info OOB prepared at queue time
- kept `receivedPacket` unchanged, intentionally avoiding a larger per-packet struct expansion on the main receive hot path
- added focused tests on OOB-capable platforms to verify that queued wrappers and delayed close packets preserve the cached OOB bytes

Why this shape:

- this is a narrow follow-up to the send-side OOB work: it removes a few remaining asynchronous `info.OOB()` rebuilds without pushing caching into the receive hot path
- storing cached OOB on queue items is lower risk than adding an OOB cache directly to `packetInfo`, which would increase `receivedPacket` size for every incoming packet

Expected effect:

- fewer repeated `packetInfo.OOB()` allocations on delayed control-packet send paths
- cleaner separation between "capture packet metadata at enqueue time" and "serialize packet to socket later"

Verification:

- `git diff --check` passed on 2026-04-19 after this round
- `go test` could not be run on 2026-04-19 because `go` was not installed in `PATH` in this environment

Follow-up still worth doing:

- when a Go toolchain is available, run focused tests for the new OOB-caching helpers and surrounding server / transport paths
- if benchmarks show these control-packet paths still matter, consider whether other queued packet types should capture send metadata the same way

### 2026-04-19 - Round 4: HTTP/3 datagram receive queue ring buffer

Status:

- implemented
- verification partially blocked by missing Go toolchain in this environment

Files touched:

- `http3/datagram.go`
- `http3/datagram_test.go`

What changed:

- replaced the HTTP/3 datagram receive queue's `[][]byte` slice with a preinitialized ring buffer
- kept the queue bounded at `streamDatagramQueueLen`, so the externally visible drop behavior stays the same
- removed the repeated `queue = queue[1:]` head-slice churn on every receive
- added a focused wrap-around ordering test to pin the new queue behavior

Why this shape:

- this follows the existing local TODO in `http3/datagram.go`
- the change is tightly scoped to the HTTP/3 datagram receive helper and does not affect QUIC DATAGRAM ownership semantics
- compared with optimizing `http3/conn.go` datagram send copies, this is a lower-risk step because it doesn't change buffer lifetime expectations

Expected effect:

- lower queue bookkeeping overhead for HTTP/3 datagram receive paths
- less chance of retaining older backing arrays through repeated head-slicing
- stable FIFO behavior under enqueue / dequeue wrap-around

Verification:

- `git diff --check` passed on 2026-04-19 after this round
- `go test` could not be run on 2026-04-19 because `go` was not installed in `PATH` in this environment

Follow-up still worth doing:

- when a Go toolchain is available, run focused HTTP/3 datagram tests
- revisit `http3/conn.go` send-side datagram copy reduction if profiling shows HTTP/3 datagrams are still allocation-heavy

### 2026-04-19 - Round 5: QUIC datagram queue ring buffers

Status:

- implemented
- verification partially blocked by missing Go toolchain in this environment

Files touched:

- `datagram_queue.go`
- `datagram_queue_test.go`

What changed:

- replaced the QUIC datagram receive queue's `[][]byte` container with a ring buffer while keeping the existing payload copy semantics unchanged
- preinitialized both the send and receive ring buffers in `newDatagramQueue` to their bounded capacities, avoiding incremental growth on the first pushes
- kept the queue limits and blocking / drop behavior unchanged
- added a wrap-around FIFO test for the receive queue

Why this shape:

- this is a structural queue optimization only; it deliberately does not try to pool or reuse received datagram payload buffers
- that keeps buffer ownership semantics unchanged while still removing repeated head-slice churn

Expected effect:

- lower queue bookkeeping overhead on QUIC DATAGRAM receive paths
- fewer small allocations while filling the bounded send queue
- less chance of retaining older receive-queue backing arrays through repeated `queue = queue[1:]` slicing

Verification:

- `git diff --check` passed on 2026-04-19 after this round
- `go test` could not be run on 2026-04-19 because `go` was not installed in `PATH` in this environment

Follow-up still worth doing:

- when a Go toolchain is available, run focused DATAGRAM queue tests
- if profiling still highlights DATAGRAM traffic, revisit payload buffer reuse separately with explicit ownership rules

### 2026-04-19 - Round 6: shared batch receive timestamp in sys_conn_oob

Status:

- implemented
- verification partially blocked by missing Go toolchain in this environment

Files touched:

- `sys_conn_oob.go`
- `sys_conn_oob_test.go`

What changed:

- captured a single `time.Now()` timestamp after each successful `ReadBatch` call and reused it for all packets returned from that batch
- added a focused test that verifies packets pulled from the same mocked batch share the same receive timestamp even when `ReadPacket` calls are separated in time

Why this shape:

- this is a direct reduction of per-packet work on the batched UDP receive path
- the semantic tradeoff is explicit and small: timestamps are now batch-granularity instead of per-`ReadPacket` call, which is a better fit for batched reads anyway

Expected effect:

- fewer `time.Now()` calls on the highest-throughput receive path
- receive timestamps that better reflect "batch arrival time" than "when the caller happened to pull the next message out of the batch"

Verification:

- `git diff --check` passed on 2026-04-19 after this round
- `go test` could not be run on 2026-04-19 because `go` was not installed in `PATH` in this environment

Follow-up still worth doing:

- when a Go toolchain is available, run focused `sys_conn_oob` tests
- if profiling still points to receive-side metadata work, revisit whether any OOB parsing can be tightened further without sacrificing clarity

### 2026-04-19 - Round 7: HTTP/3 datagram send fast path

Status:

- implemented
- verification partially blocked by missing Go toolchain in this environment

Files touched:

- `connection.go`
- `http3/conn.go`
- `http3/conn_test.go`

What changed:

- factored QUIC datagram sending in `connection.go` through a shared helper that can prepend an internal header slice and still perform only one payload copy into the queued DATAGRAM frame
- added an optional `SendDatagramWithHeader` fast path on quic-go's internal connection implementation
- updated HTTP/3 datagram sending to use that fast path when the underlying QUIC connection supports it, and fall back to the public `SendDatagram([]byte)` path otherwise
- added a focused HTTP/3 test for the fast-path behavior while preserving the existing fallback-path test

Why this shape:

- this avoids changing the public QUIC connection interface
- non-quic-go implementations keep working through the fallback path
- the optimization targets the specific extra allocation / copy already called out by the HTTP/3 TODO, without changing datagram ownership semantics

Expected effect:

- one fewer temporary allocation / copy on HTTP/3 datagram sends when using quic-go's own connection implementation
- preserved compatibility for callers using other QUIC implementations behind the same interface

Verification:

- `git diff --check` passed on 2026-04-19 after this round
- `go test` could not be run on 2026-04-19 because `go` was not installed in `PATH` in this environment

Follow-up still worth doing:

- when a Go toolchain is available, run focused HTTP/3 connection / datagram tests
- if datagram send profiling still points here, consider whether a similar segmented-copy helper is worthwhile elsewhere, but avoid broadening it without measurements
