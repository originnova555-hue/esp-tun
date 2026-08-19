package loadtest

import (
	"context"
	"testing"
	"time"
)

func TestBulkTestBasicTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bt := &BulkTest{
		ServerAddr: "127.0.0.1:0",
		Duration:   1 * time.Second,
		BufferSize: 8192,
	}

	result, err := bt.Run(ctx)
	if err != nil {
		t.Fatalf("bulk test failed: %v", err)
	}

	if result.BytesSent == 0 {
		t.Errorf("expected bytes sent, got 0")
	}

	if result.Throughput <= 0 {
		t.Errorf("expected positive throughput, got %.2f", result.Throughput)
	}

	if result.Duration == 0 {
		t.Errorf("expected duration > 0")
	}
}

func TestBulkTestMultipleRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		bt := &BulkTest{
			ServerAddr: "127.0.0.1:0",
			Duration:   500 * time.Millisecond,
			BufferSize: 4096,
		}

		result, err := bt.Run(ctx)
		if err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}

		if result.BytesSent == 0 {
			t.Errorf("run %d: no bytes sent", i)
		}

		t.Logf("Run %d: %.2f Gbps", i, result.Throughput)
	}
}

func TestManyFlowTestBasic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mft := &ManyFlowTest{
		ServerAddr: "127.0.0.1:0",
		NumFlows:   10,
		Duration:   2 * time.Second,
		BurstSize:  5,
	}

	result, err := mft.Run(ctx)
	if err != nil {
		t.Fatalf("flow test failed: %v", err)
	}

	if result.TotalFlows != 10 {
		t.Errorf("expected 10 flows, got %d", result.TotalFlows)
	}

	if result.SuccessFlows == 0 {
		t.Errorf("expected successful flows, got 0")
	}

	t.Logf("Success rate: %d/%d (%.1f%%)",
		result.SuccessFlows, result.TotalFlows,
		float64(result.SuccessFlows)/float64(result.TotalFlows)*100)
}

func TestManyFlowTestConcurrency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mft := &ManyFlowTest{
		ServerAddr: "127.0.0.1:0",
		NumFlows:   50,
		Duration:   5 * time.Second,
		BurstSize:  10,
	}

	result, err := mft.Run(ctx)
	if err != nil {
		t.Fatalf("flow test failed: %v", err)
	}

	successRate := float64(result.SuccessFlows) / float64(result.TotalFlows) * 100
	if successRate < 80.0 {
		t.Errorf("success rate too low: %.1f%%", successRate)
	}

	t.Logf("Concurrency test: %d/%d flows succeeded (%.1f%%)",
		result.SuccessFlows, result.TotalFlows, successRate)
}
