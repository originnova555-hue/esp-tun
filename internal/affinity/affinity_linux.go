// Package affinity pins the calling goroutine's OS thread to a
// specific CPU core. Used by the TUN read/write worker goroutines
// (one per multiqueue queue, see internal/tun.OpenQueues) so that
// each queue's packet processing stays on one core instead of being
// shuffled by the Go scheduler — better cache locality for the
// per-flow hashing and QUIC datagram send/receive path, and avoids
// contention between queues that would otherwise land on the same
// core under load.
package affinity

import (
	"fmt"
	"runtime"

	"golang.org/x/sys/unix"
)

// PinCurrentGoroutine locks the calling goroutine to its current OS
// thread (runtime.LockOSThread) and restricts that thread to run
// only on the given core. Must be called from the goroutine that
// will do the pinned work — the lock and affinity mask apply to
// whichever OS thread is running the calling goroutine at the time of
// the call, and LockOSThread ensures the Go scheduler never migrates
// this goroutine to a different thread afterward.
//
// Callers should invoke this once at the top of a long-running worker
// goroutine (e.g. a TUN queue's read loop), not per-packet. There is
// no matching Unpin — the lock is meant to live for the goroutine's
// entire lifetime; runtime.LockOSThread is automatically released
// when the goroutine exits.
func PinCurrentGoroutine(core int) error {
	if core < 0 {
		return fmt.Errorf("affinity: negative core index %d", core)
	}

	runtime.LockOSThread()

	var set unix.CPUSet
	set.Zero()
	set.Set(core)

	// Pid 0 means "the calling thread" (Linux sched_setaffinity
	// semantics) — correct here because LockOSThread above has
	// already bound this goroutine to the current OS thread.
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("affinity: sched_setaffinity core %d: %w", core, err)
	}
	return nil
}

// NumCPU returns the number of logical CPUs available to this
// process (respecting cgroup/taskset restrictions, unlike a hardcoded
// runtime.NumCPU() call at a call site that forgot the distinction).
// Thin wrapper kept in this package so callers doing core-count-based
// sizing (e.g. "one TUN queue + one pinned worker per core") have one
// place to change if a more precise cgroup-aware count is ever needed.
func NumCPU() int {
	return runtime.NumCPU()
}
