# Concurrency and Race-Free Patterns in ObjectFS

This document outlines best practices for writing race-free concurrent code in
ObjectFS, based on lessons learned from the race condition audit.

## Table of Contents

1. [Overview](#overview)
2. [Common Race Condition Patterns](#common-race-condition-patterns)
3. [Lock Management](#lock-management)
4. [Safe Patterns](#safe-patterns)
5. [Testing for Race Conditions](#testing-for-race-conditions)
6. [Real-World Examples](#real-world-examples)

## Overview

Race conditions occur when multiple goroutines access shared memory
concurrently, and at least one access is a write. Go's race detector is an
invaluable tool for finding these issues during development.

**Key Principles:**

- **Minimize lock scope**: Hold locks for the shortest time possible
- **Never upgrade locks**: You cannot upgrade an RLock to a Lock
- **Copy under lock**: When passing data to functions, copy it while holding the lock
- **Document lock requirements**: Clearly state whether functions expect locks to be held

## Common Race Condition Patterns

### Pattern 1: Slice Append Without Lock

**❌ WRONG:**

```go
type Monitor struct {
    mu sync.RWMutex
    items []Item
}

func (m *Monitor) addItem(item Item) {
    // Race: appending without lock!
    m.items = append(m.items, item)
}
```

**✅ CORRECT:**

```go
func (m *Monitor) addItem(item Item) {
    m.mu.Lock()
    m.items = append(m.items, item)
    m.mu.Unlock()
}
```

**Why it matters:** Slice append can cause reallocation, making concurrent reads crash or see corrupted data.

### Pattern 2: Lock Upgrade Deadlock

**❌ WRONG:**

```go
func (m *Monitor) processData() {
    m.mu.RLock()
    defer m.mu.RUnlock()

    data := m.data
    if needsUpdate(data) {
        m.mu.Lock()  // DEADLOCK! Can't upgrade RLock to Lock
        m.data = update(data)
        m.mu.Unlock()
    }
}
```

**✅ CORRECT:**

```go
func (m *Monitor) processData() {
    m.mu.RLock()
    data := m.data
    needsUpdate := needsUpdate(data)
    m.mu.RUnlock()

    if needsUpdate {
        m.mu.Lock()
        m.data = update(m.data)  // Re-read under write lock
        m.mu.Unlock()
    }
}
```

**Why it matters:** You cannot upgrade from a read lock to a write lock. Always release the RLock before acquiring a Lock.

### Pattern 3: Calling Locking Functions While Holding Lock

**❌ WRONG:**

```go
func (m *Monitor) analyze() {
    m.mu.RLock()
    defer m.mu.RUnlock()

    if m.hasIssue() {
        m.recordIssue()  // This acquires m.mu.Lock() = DEADLOCK!
    }
}

func (m *Monitor) recordIssue() {
    m.mu.Lock()  // Deadlock if caller holds RLock
    m.issues = append(m.issues, issue)
    m.mu.Unlock()
}
```

**✅ CORRECT (Option 1 - Release lock early):**

```go
func (m *Monitor) analyze() {
    m.mu.RLock()
    hasIssue := m.hasIssue()
    m.mu.RUnlock()

    if hasIssue {
        m.recordIssue()  // Safe - no lock held
    }
}
```

**✅ CORRECT (Option 2 - Collect data, process later):**

```go
func (m *Monitor) analyze() {
    m.mu.RLock()
    var issuesToRecord []Issue

    for _, item := range m.items {
        if item.hasIssue() {
            issuesToRecord = append(issuesToRecord, item.issue)
        }
    }
    m.mu.RUnlock()

    // Record all issues outside the lock
    for _, issue := range issuesToRecord {
        m.recordIssue(issue)
    }
}
```

## Lock Management

### Lock Duration

**Principle:** Hold locks for the **minimum** time necessary.

**❌ WRONG (Lock held too long):**

```go
func (m *Monitor) processItems() {
    m.mu.Lock()
    defer m.mu.Unlock()

    for _, item := range m.items {
        result := expensiveOperation(item)  // Blocks all other goroutines!
        m.results = append(m.results, result)
    }
}
```

**✅ CORRECT (Minimize lock scope):**

```go
func (m *Monitor) processItems() {
    m.mu.RLock()
    items := make([]Item, len(m.items))
    copy(items, m.items)
    m.mu.RUnlock()

    var results []Result
    for _, item := range items {
        result := expensiveOperation(item)  // No lock held
        results = append(results, result)
    }

    m.mu.Lock()
    m.results = append(m.results, results...)
    m.mu.Unlock()
}
```

### Read vs Write Locks

- **RLock (Read Lock):** Multiple goroutines can hold simultaneously. Use for read-only operations.
- **Lock (Write Lock):** Exclusive access. Use for modifications.

**Guidelines:**

- Use `RLock` for reads when no writes occur
- Use `Lock` for writes or read-modify-write operations
- Never try to upgrade `RLock` to `Lock`

**Example:**

```go
// Read operation - use RLock
func (m *Monitor) GetStats() Stats {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.stats  // Reading only
}

// Write operation - use Lock
func (m *Monitor) UpdateStats(delta int) {
    m.mu.Lock()
    defer m.mu.Unlock()
    m.stats.Count += delta  // Modifying
}

// Read-modify-write - use Lock
func (m *Monitor) IncrementIfPositive() bool {
    m.mu.Lock()
    defer m.mu.Unlock()

    if m.value > 0 {  // Read
        m.value++      // Write
        return true
    }
    return false
}
```

### Defer vs Explicit Unlock

**Use `defer` when:**

- Function has multiple return paths
- Lock should be held for entire function
- Error handling might cause early returns

**Use explicit `Unlock()` when:**

- Lock only needed for part of function
- Minimizing lock duration is critical
- Lock/unlock boundaries are clear

**Example:**

```go
// defer - good for simple cases
func (m *Monitor) GetValue() int {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.value
}

// explicit - good for minimizing lock scope
func (m *Monitor) ProcessData() Result {
    m.mu.RLock()
    data := m.data
    m.mu.RUnlock()  // Explicit - lock released early

    return expensiveComputation(data)
}
```

## Safe Patterns

### Pattern: Copy-and-Release

When calling functions that may need locks, copy data under lock and release before calling:

```go
func (mm *MemoryMonitor) analyzeMemory() {
    // Acquire lock
    mm.mu.RLock()

    // Copy data
    baseline := mm.baselineSample
    current := mm.currentSample

    // Release lock ASAP
    mm.mu.RUnlock()

    // Process data without lock
    if needsAlert(baseline, current) {
        mm.generateAlert(...)  // This can safely acquire locks internally
    }
}
```

### Pattern: Collect-Then-Process

Collect items to process while holding lock, then process outside lock:

```go
func (mm *MemoryMonitor) analyzeMemory() {
    mm.mu.RLock()
    baseline := mm.baselineSample
    current := mm.currentSample
    mm.mu.RUnlock()

    // Collect alerts to generate
    var alertsToGenerate []AlertInfo

    if current.Memory > threshold {
        alertsToGenerate = append(alertsToGenerate, AlertInfo{...})
    }

    // Generate alerts (may acquire locks internally)
    for _, alert := range alertsToGenerate {
        mm.generateAlert(alert.Type, alert.Message, ...)
    }
}
```

### Pattern: Internal Locking

Functions that modify shared state should acquire locks internally:

```go
// generateAlert acquires lock internally
func (mm *MemoryMonitor) generateAlert(...) {
    alert := MemoryAlert{...}

    mm.mu.Lock()
    mm.alerts = append(mm.alerts, alert)
    mm.mu.Unlock()

    mm.logger.Warn(...)  // External call outside lock
}
```

**Documentation is key:**

```go
// generateAlert generates a memory alert.
// This function acquires mm.mu internally - do NOT call while holding lock.
func (mm *MemoryMonitor) generateAlert(...) {
```

### Pattern: Atomic Operations

For simple counters, consider using `sync/atomic`:

```go
type Monitor struct {
    requestCount atomic.Int64  // No lock needed!
}

func (m *Monitor) IncrementRequests() {
    m.requestCount.Add(1)
}

func (m *Monitor) GetRequestCount() int64 {
    return m.requestCount.Load()
}
```

## Testing for Race Conditions

### Using the Race Detector

**Run tests with race detector:**

```bash
go test -race ./...
go test -race ./pkg/memmon/...
go test -race -run TestConcurrentAccess
```

**Run with race detector in short mode:**

```bash
go test -race -short ./...
```

### Writing Concurrent Tests

Test concurrent access patterns explicitly:

```go
func TestMemoryMonitor_ConcurrentAccess(t *testing.T) {
    monitor := NewMemoryMonitor(config)

    ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
    defer cancel()

    // Start background monitoring
    if err := monitor.Start(ctx); err != nil {
        t.Fatalf("Failed to start monitor: %v", err)
    }

    // Concurrent access from multiple goroutines
    done := make(chan bool, 10)
    for i := 0; i < 10; i++ {
        go func() {
            for j := 0; j < 10; j++ {
                monitor.GetStats()          // Reads
                monitor.GetAlerts()         // Reads
                monitor.IncrementObject("test", 100)  // Writes
                monitor.DecrementObject("test", 100)  // Writes
                time.Sleep(5 * time.Millisecond)
            }
            done <- true
        }()
    }

    // Wait for all goroutines
    for i := 0; i < 10; i++ {
        <-done
    }

    if err := monitor.Stop(); err != nil {
        t.Logf("Error stopping monitor: %v", err)
    }

    t.Log("Concurrent access test completed successfully")
}
```

### CI/CD Integration

Ensure race detector runs in CI:

```yaml
# .github/workflows/test.yml
- name: Test with race detector
  run: go test -race -short ./...
```

## Real-World Examples

### Example 1: Memory Monitor (Fixed)

**Issue:** Deadlock when `analyzeMemory()` called `generateAlert()` while holding RLock.

**Solution:** Collect alert data under lock, release lock, then generate alerts.

**Before:**

```go
func (mm *MemoryMonitor) analyzeMemory() {
    mm.mu.RLock()
    defer mm.mu.RUnlock()  // Held entire time!

    if mm.currentSample.Alloc > threshold {
        mm.generateAlert(...)  // generateAlert acquires Lock = DEADLOCK!
    }
}
```

**After:**

```go
func (mm *MemoryMonitor) analyzeMemory() {
    mm.mu.RLock()
    baseline := mm.baselineSample
    current := mm.currentSample
    mm.mu.RUnlock()  // Released early!

    var alertsToGenerate []AlertInfo
    if current.Alloc > threshold {
        alertsToGenerate = append(alertsToGenerate, ...)
    }

    for _, alert := range alertsToGenerate {
        mm.generateAlert(...)  // Safe - no lock held
    }
}
```

### Example 2: LRU Cache Cleanup

**Issue:** Not properly nil-ing slices and references for GC.

**Solution:** Explicitly nil out references before deletion.

**Before:**

```go
func (c *LRUCache) removeItem(key string) {
    item := c.items[key]
    c.evictList.Remove(item.element)
    delete(c.items, key)
}
```

**After:**

```go
func (c *LRUCache) removeItem(key string) {
    item := c.items[key]

    if item.element != nil {
        c.evictList.Remove(item.element)
        item.element = nil  // Help GC
    }

    if item.data != nil {
        item.data = nil  // Help GC
    }

    delete(c.items, key)
}
```

## Quick Reference Checklist

When reviewing concurrent code, check:

- [ ] All shared state access protected by locks
- [ ] No lock upgrades (RLock → Lock)
- [ ] Locks released before calling functions that may lock
- [ ] Lock scope minimized (copy data, release lock, process)
- [ ] Slice append operations protected by Lock (not RLock)
- [ ] Map access protected by locks
- [ ] Tests run with `-race` flag
- [ ] Concurrent access patterns tested
- [ ] Lock requirements documented in comments
- [ ] defer vs explicit unlock chosen appropriately

## Additional Resources

- [Go Race Detector](https://go.dev/doc/articles/race_detector)
- [Effective Go - Concurrency](https://go.dev/doc/effective_go#concurrency)
- [Go Memory Model](https://go.dev/ref/mem)
- [sync package documentation](https://pkg.go.dev/sync)

## Related Documentation

- [Memory Monitoring and Leak Detection](./memory-monitoring.md)
- [Error Handling and Recovery](./error-handling-recovery.md)
- [Testing Guide](./testing.md)
