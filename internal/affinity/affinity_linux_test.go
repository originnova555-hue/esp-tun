package affinity

import (
	"runtime"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPinCurrentGoroutineSetsAffinity(t *testing.T) {
	if NumCPU() < 2 {
		t.Skip("skipping: needs at least 2 CPUs to verify a restrictive mask")
	}

	done := make(chan error, 1)
	var gotSet unix.CPUSet
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer runtime.UnlockOSThread()
		if err := PinCurrentGoroutine(0); err != nil {
			done <- err
			return
		}
		if err := unix.SchedGetaffinity(0, &gotSet); err != nil {
			done <- err
			return
		}
		done <- nil
	}()
	wg.Wait()

	if err := <-done; err != nil {
		t.Fatalf("PinCurrentGoroutine: %v", err)
	}
	if !gotSet.IsSet(0) {
		t.Errorf("core 0 not set in resulting affinity mask")
	}
	if gotSet.Count() != 1 {
		t.Errorf("affinity mask has %d cores set, want exactly 1 (pinned to core 0)", gotSet.Count())
	}
}

func TestPinCurrentGoroutineRejectsNegativeCore(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		defer runtime.UnlockOSThread()
		done <- PinCurrentGoroutine(-1)
	}()
	if err := <-done; err == nil {
		t.Fatal("expected error for negative core index")
	}
}

func TestNumCPUMatchesRuntime(t *testing.T) {
	if NumCPU() != runtime.NumCPU() {
		t.Errorf("NumCPU() = %d, want %d", NumCPU(), runtime.NumCPU())
	}
}
