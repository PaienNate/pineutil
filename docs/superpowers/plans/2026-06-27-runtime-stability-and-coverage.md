# Runtime Stability And Coverage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the confirmed runtime bugs in `evsocket`, `batchy`, `ftlserver`, and `syncmap`, and add enough automated coverage to prove connection lifecycle, shutdown, buffering, and concurrent map behavior are correct.

**Architecture:** Keep public APIs stable where possible, but tighten internal lifecycle management. `evsocket` gets the largest change set: explicit connection/session ownership, safe event payload snapshots, deterministic queue shutdown behavior, and test-backed client reconnect handling. The other packages get targeted root-cause fixes plus concurrency or shutdown regression tests.

**Tech Stack:** Go 1.23, `testing`, `httptest`, `gorilla/websocket`, `sync`, `context`, `time`, `go test -race`

## Global Constraints

- Preserve exported package APIs unless a change is required to make broken behavior observable and testable.
- Add tests before changing behavior for every confirmed bug path.
- Prefer package-local helpers over broad refactors.
- Run targeted tests first, then package tests, then `go test ./... -race -count=1`.
- Add enough tests to cover success paths, failure paths, reconnect/shutdown timing, and concurrent access.
- Do not spend time renaming `jsontime` package or fixing `.golangci.toml` in this plan; those are cleanup items, not runtime defects.

---

## File Map

- Modify: `evsocket/safeListener.go`
- Modify: `evsocket/socketio.go`
- Create: `evsocket/safeListener_test.go`
- Create: `evsocket/socketio_server_test.go`
- Create: `evsocket/socketio_client_test.go`
- Create: `evsocket/socketio_queue_test.go`
- Modify: `batchy/chan_batcher.go`
- Create: `batchy/chan_batcher_test.go`
- Modify: `ftlserver/endless.go`
- Create: `ftlserver/endless_test.go`
- Modify: `syncmap/syncmap.go`
- Modify: `syncmap/syncmap_test.go`

## Test Matrix

- `evsocket`
  - concurrent registration for one event keeps every callback
  - `EventPayload.SocketAttributes` is a snapshot and does not race with `SetAttribute`
  - server-side connection close triggers a single disconnect path
  - client reconnect re-enters pool and can receive `EmitTo`
  - disconnect after queued writes does not deadlock later `Emit`
  - queued messages either flush before shutdown or produce a deterministic drop notification
  - heartbeat/read loop still delivers text/binary/ping/pong/close events
- `batchy`
  - `Stop()` flushes buffered items already accepted by workers
  - `Stop()` drains queued items accepted before shutdown
  - `Add()` returns an error, not false success, when racing shutdown
  - normal batching and timeout batching still work after fixes
- `ftlserver`
  - child listener restoration only constructs one listener path
  - non-restored child path does not call `net.FileListener(nil)`
- `syncmap`
  - sequential semantics remain unchanged
  - concurrent `Store/Delete/Clear` does not permanently undercount after operations settle
  - JSON marshal/unmarshal still works after internal changes

### Task 1: Build `evsocket` Regression Tests First

**Files:**
- Create: `evsocket/safeListener_test.go`
- Create: `evsocket/socketio_server_test.go`
- Create: `evsocket/socketio_client_test.go`
- Create: `evsocket/socketio_queue_test.go`
- Test: `evsocket/*.go`

**Interfaces:**
- Consumes: `(*SocketInstance).On`, `(*SocketInstance).New`, `(*SocketInstance).NewClient`, `(*WebsocketWrapper).ClientConnect`, `(*WebsocketWrapper).Emit`, `(*SocketInstance).EmitTo`, `EventDisconnect`, `EventMessage`, `EventError`
- Produces: failing tests that pin current regressions before implementation

- [ ] **Step 1: Add a concurrent listener registration test**

Create `evsocket/safeListener_test.go` with a test that starts `N` goroutines, each calling `sm.On("message", cb)` on the same `SocketInstance`, then triggers one `fireEvent` and asserts exactly `N` callbacks ran.

- [ ] **Step 2: Add attribute snapshot and disconnect behavior tests**

Create `evsocket/socketio_server_test.go` using `httptest.NewServer` plus `websocket.Upgrader` or the package's own handler. Add tests that:
- set attributes while event callbacks read `SocketAttributes`
- assert callbacks receive a stable snapshot copy
- assert only one disconnect event fires for a single close sequence

- [ ] **Step 3: Add client reconnect and pool visibility tests**

Create `evsocket/socketio_client_test.go` with a local websocket server. Add tests that:
- connect a client created by `NewClient`
- force a disconnect from the server side
- reconnect the same wrapper
- call `sm.EmitTo(client.GetUUID(), payload)` and assert the client receives it after reconnect

- [ ] **Step 4: Add queue shutdown tests**

Create `evsocket/socketio_queue_test.go` with tests that:
- queue a message, force a disconnect, then call `Emit()` again and assert the call returns promptly instead of blocking forever
- verify queued messages at shutdown either get delivered or generate a deterministic error/drop signal

- [ ] **Step 5: Run the package tests to capture current failures**

Run: `go test ./evsocket -count=1`

Expected: failing tests for lost callbacks, reconnect pool visibility, and queue/disconnect behavior.

### Task 2: Fix `evsocket` Listener Safety And Event Payload Ownership

**Files:**
- Modify: `evsocket/safeListener.go`
- Modify: `evsocket/socketio.go`
- Test: `evsocket/safeListener_test.go`
- Test: `evsocket/socketio_server_test.go`

**Interfaces:**
- Consumes: existing `safeListeners` storage and `WebsocketWrapper.SetAttribute`
- Produces: atomic listener registration and copied event payload attributes

- [ ] **Step 1: Make listener registration atomic**

Replace the read/append/store slice update in `evsocket/safeListener.go` with a synchronization strategy that cannot lose callbacks under concurrent registration. Keep the public `set(event, callback)` and `get(event)` behavior unchanged.

- [ ] **Step 2: Snapshot attributes before firing events**

In `evsocket/socketio.go`, copy `kws.attributes` under `mu.RLock()` before building `EventPayload`, and pass the copy in `SocketAttributes`.

- [ ] **Step 3: Keep `get()` returning a defensive copy**

Retain the current `append([]eventCallback{}, callbacks...)` pattern or equivalent so callers cannot mutate the stored callback slice.

- [ ] **Step 4: Re-run targeted tests**

Run: `go test ./evsocket -run 'TestSafeListeners|TestEventPayload' -count=1`

Expected: listener registration and attribute snapshot tests pass.

- [ ] **Step 5: Run race detection for the package**

Run: `go test ./evsocket -race -count=1`

Expected: no race reports from event callback tests.

### Task 3: Fix `evsocket` Client Session Lifecycle And Reconnect Semantics

**Files:**
- Modify: `evsocket/socketio.go`
- Test: `evsocket/socketio_client_test.go`
- Test: `evsocket/socketio_server_test.go`

**Interfaces:**
- Consumes: `NewClient`, `ClientConnect`, `disconnected`, pool registration helpers
- Produces: deterministic reconnect semantics where the active session is the only one allowed to close itself, and reconnect always restores addressability through `EmitTo`

- [ ] **Step 1: Introduce active-session ownership inside `WebsocketWrapper`**

Track a per-connection session token or generation number inside `WebsocketWrapper`. Every goroutine spawned by `run()` captures that token, and `disconnected()` only tears down the session if the token still matches the currently active one.

- [ ] **Step 2: Stop reusing stale teardown primitives across reconnects**

Replace the current “reset `sync.Once` and replace `done`” reconnect pattern with per-session teardown state. The new session must get its own close signal, and old goroutines must be unable to close the new session.

- [ ] **Step 3: Re-register reconnecting clients into the pool**

On every successful `ClientConnect`, ensure the wrapper is present in the manager's addressable client path, not only on first connection.

- [ ] **Step 4: Keep UUID stable across reconnects**

Retain existing UUID if present so reconnect remains addressable by previous callers and tests can prove `EmitTo` still works after reconnect.

- [ ] **Step 5: Run reconnect-focused tests**

Run: `go test ./evsocket -run 'TestClientReconnect|TestEmitToAfterReconnect|TestDisconnectSingleFire' -count=1`

Expected: reconnect tests pass and no stale disconnect tears down the new session.

### Task 4: Fix `evsocket` Queue Shutdown, Read Loop, And Heartbeat Behavior

**Files:**
- Modify: `evsocket/socketio.go`
- Test: `evsocket/socketio_queue_test.go`
- Test: `evsocket/socketio_server_test.go`
- Test: `evsocket/socketio_client_test.go`

**Interfaces:**
- Consumes: `write`, `send`, `read`, `run`, heartbeat callbacks
- Produces: non-blocking post-disconnect behavior, deterministic queue shutdown, and blocking read loop with protocol-correct heartbeat

- [ ] **Step 1: Make `Emit`/`write` shutdown-aware**

Change the internal write path so a disconnected or shutting-down wrapper does not block forever on `queue <- message`. The call must either enqueue successfully for the active session or fail fast in a way tests can observe.

- [ ] **Step 2: Define queued-message shutdown semantics**

Before `send` exits, attempt to flush already accepted messages for the active session. Messages that cannot be delivered must be surfaced through a deterministic error/drop event or callback path so the loss is observable.

- [ ] **Step 3: Convert `read()` to a blocking read loop**

Remove the outer `time.NewTicker(ReadTimeout)` polling pattern. Use a normal blocking `ReadMessage()` loop with close handling that cooperates with session teardown.

- [ ] **Step 4: Replace unsolicited pong heartbeat with ping-based liveness**

Send `PingMessage` on the heartbeat ticker, keep the `OnPingReceived`/`OnPongReceived` callbacks working, and ensure idle peers are still detectable.

- [ ] **Step 5: Re-run queue and lifecycle tests**

Run: `go test ./evsocket -run 'TestQueue|TestDisconnect|TestHeartbeat|TestMessageDelivery' -count=1`

Expected: no deadlocks, disconnect-related tests pass, message delivery tests still pass.

- [ ] **Step 6: Stress the package under race detection**

Run: `go test ./evsocket -race -count=20`

Expected: stable pass without race reports or intermittent deadlocks.

### Task 5: Add `batchy` Regression Tests Before Fixing Shutdown Behavior

**Files:**
- Create: `batchy/chan_batcher_test.go`
- Test: `batchy/chan_batcher.go`

**Interfaces:**
- Consumes: `NewChanBatcher`, `BatchConfig`, `Add`, `Stop`
- Produces: failing tests for shutdown races and preserved batching semantics

- [ ] **Step 1: Add a buffered-item flush test**

Create a processor that records every item it receives. Add fewer than `BatchSize` items, call `Stop()`, and assert every accepted item is eventually observed.

- [ ] **Step 2: Add a queued-item drain test**

Configure a small worker count and queue enough items to sit in the queue while stopping. Assert all items accepted before `Stop()` are processed exactly once.

- [ ] **Step 3: Add an `Add` vs `Stop` race test**

Run `Add()` and `Stop()` concurrently in a loop. Assert `Add()` never reports success for an item that is neither processed nor explicitly rejected.

- [ ] **Step 4: Add baseline batching tests**

Verify size-based batching and timeout-based batching continue to work after the shutdown changes.

- [ ] **Step 5: Run the package tests to capture current failures**

Run: `go test ./batchy -count=1`

Expected: at least the shutdown-related tests fail against current behavior.

### Task 6: Fix `batchy` Shutdown And `Add` Error Semantics

**Files:**
- Modify: `batchy/chan_batcher.go`
- Test: `batchy/chan_batcher_test.go`

**Interfaces:**
- Consumes: worker loop, `Stop`, `Add`
- Produces: “accepted means eventually processed” semantics and accurate `Add` failures during shutdown

- [ ] **Step 1: Validate configuration before derived sizing**

Move `PoolSize`, `Timeout`, and `processor` validation ahead of queue-size derivation so invalid configs fail before any dependent math.

- [ ] **Step 2: Make `Add()` return explicit shutdown errors**

Replace the swallowed panic path with deterministic rejection when shutdown has started. `Add()` must not return `nil` for a dropped item.

- [ ] **Step 3: Flush worker-local buffers during shutdown**

When workers observe shutdown, process remaining buffered items before exiting.

- [ ] **Step 4: Drain accepted queued items during shutdown**

Ensure items already accepted into the shared queue before `Stop()` are either processed before worker exit or explicitly rejected before `Add()` returns.

- [ ] **Step 5: Re-run targeted and race tests**

Run: `go test ./batchy -race -count=10`

Expected: shutdown tests pass without flakes or false-success `Add()` results.

### Task 7: Add And Fix `ftlserver` Listener Restoration Tests

**Files:**
- Create: `ftlserver/endless_test.go`
- Modify: `ftlserver/endless.go`

**Interfaces:**
- Consumes: `(*endlessServer).getListener`
- Produces: child-mode listener acquisition that never calls `net.FileListener(nil)` and never duplicates the same path twice

- [ ] **Step 1: Add a focused test seam for child listener acquisition**

Introduce package-level indirection for `net.Listen` / `net.FileListener` only if needed to test `getListener` deterministically. Keep the seam unexported and local to `ftlserver`.

- [ ] **Step 2: Write failing tests for child-mode branches**

Add tests that simulate:
- `srv.isChild == true` with `existed == true`: `net.FileListener` is called exactly once
- `srv.isChild == true` with `existed == false`: fallback uses `net.Listen` and never passes nil to `net.FileListener`

- [ ] **Step 3: Fix the branching in `getListener`**

Make the child restore path mutually exclusive: use `net.FileListener` only when restoring an inherited descriptor; otherwise use `net.Listen` only.

- [ ] **Step 4: Run targeted tests**

Run: `go test ./ftlserver -run 'TestGetListener' -count=1`

Expected: child listener tests pass.

- [ ] **Step 5: Run package build/tests**

Run: `go test ./ftlserver/... -count=1`

Expected: pass on current platform.

### Task 8: Fix `syncmap` Count Consistency And Expand Concurrency Coverage

**Files:**
- Modify: `syncmap/syncmap.go`
- Modify: `syncmap/syncmap_test.go`

**Interfaces:**
- Consumes: `Store`, `Delete`, `LoadAndDelete`, `Clear`, `Len`
- Produces: consistent post-operation counts and tests that prove no settled-state undercount remains after concurrent mutations

- [ ] **Step 1: Decide and document `Len()` semantics in code**

Implement either strict accuracy after operations settle or explicitly relaxed semantics. This plan assumes strict accuracy after quiescence.

- [ ] **Step 2: Refactor mutation accounting to avoid reinsertion undercount**

Change `Store/Delete/Clear` so no interleaving can reinsert an element without restoring the count. Use one coherent mutation path rather than independent best-effort counter edits.

- [ ] **Step 3: Extend the tests with concurrency coverage**

Add tests that loop concurrent `Store/Delete/Clear` operations, wait for goroutines to finish, then assert `Len()` matches the actual key set.

- [ ] **Step 4: Preserve JSON and basic API behavior**

Keep the existing marshal/unmarshal, `Keys`, and `Values` tests passing after internal changes.

- [ ] **Step 5: Run targeted and race tests**

Run: `go test ./syncmap -race -count=20`

Expected: pass without count drift after operations settle.

### Task 9: Full Regression And Cross-Package Verification

**Files:**
- Test: `evsocket/*.go`
- Test: `batchy/*.go`
- Test: `ftlserver/*.go`
- Test: `syncmap/*.go`

**Interfaces:**
- Consumes: all completed tasks
- Produces: verified package behavior across normal and concurrent runtime scenarios

- [ ] **Step 1: Run targeted package suites with race detection**

Run: `go test ./evsocket ./batchy ./ftlserver ./syncmap -race -count=1`

Expected: all four packages pass.

- [ ] **Step 2: Run the full repository suite**

Run: `go test ./... -race -count=1`

Expected: all packages pass.

- [ ] **Step 3: Stress the most timing-sensitive packages**

Run: `go test ./evsocket ./batchy ./syncmap -race -count=20`

Expected: no flakes, deadlocks, or race reports.

- [ ] **Step 4: Review any timing-sensitive assertions**

If a test needed sleeps, replace them with condition polling or channels before finalizing.

## Plan Self-Review

- Spec coverage: confirmed runtime bugs in `evsocket`, `batchy`, `ftlserver`, and `syncmap` each map to at least one task and one verification command.
- Testing coverage: every confirmed bug has a failing test task before its implementation task, plus full-package and race validation.
- Placeholder scan: no `TODO`/`TBD` items remain; every task lists files, expected behavior, and commands.
- Type consistency: task references only existing package APIs plus internal session/accounting changes described in the producing task.
