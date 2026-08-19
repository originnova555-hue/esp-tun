package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/originnova555-hue/esp-tun/test/loadtest"
	"github.com/originnova555-hue/esp-tun/test/profile"
)

type BenchmarkReport struct {
	Timestamp        time.Time                    `json:"timestamp"`
	BulkTestResults  *loadtest.BulkTestResult    `json:"bulk_test"`
	FlowTestResults  *loadtest.FlowTestResult    `json:"flow_test"`
	ProfileResults   map[string]*profile.ProfileResult `json:"profiles"`
	Comparison       *profile.ComparisonReport   `json:"comparison"`
}

func main() {
	var (
		outputDir     = flag.String("output", "./benchmarks", "Output directory for results")
		duration      = flag.Duration("duration", 10*time.Second, "Test duration")
		numFlows      = flag.Int("flows", 100, "Number of concurrent flows")
		configPath    = flag.String("config", "", "Config file for profiling")
		binaryPath    = flag.String("binary", "", "Binary path for profiling")
	)
	flag.Parse()

	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	report := &BenchmarkReport{
		Timestamp:      time.Now(),
		ProfileResults: make(map[string]*profile.ProfileResult),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Println("Running bulk transfer test...")
	if bulkResult, err := runBulkTest(ctx, *duration); err != nil {
		log.Printf("bulk test failed: %v", err)
	} else {
		report.BulkTestResults = bulkResult
		fmt.Printf("  Throughput: %.2f Gbps\n", bulkResult.Throughput)
	}

	fmt.Println("Running many-flow test...")
	if flowResult, err := runFlowTest(ctx, *numFlows, *duration); err != nil {
		log.Printf("flow test failed: %v", err)
	} else {
		report.FlowTestResults = flowResult
		fmt.Printf("  Success rate: %.1f%%\n", float64(flowResult.SuccessFlows)/float64(flowResult.TotalFlows)*100)
	}

	if *configPath != "" && *binaryPath != "" {
		fmt.Println("Running CPU profiling tests...")
		if err := runProfilingTests(ctx, *binaryPath, *configPath, *outputDir, report); err != nil {
			log.Printf("profiling tests failed: %v", err)
		}
	}

	if err := saveReport(*outputDir, report); err != nil {
		log.Fatalf("save report: %v", err)
	}

	fmt.Printf("\nBenchmark report saved to %s\n", filepath.Join(*outputDir, "report.json"))
	printSummary(report)
}

func runBulkTest(ctx context.Context, duration time.Duration) (*loadtest.BulkTestResult, error) {
	bt := &loadtest.BulkTest{
		ServerAddr: "127.0.0.1:0",
		Duration:   duration,
		BufferSize: 65536,
	}
	return bt.Run(ctx)
}

func runFlowTest(ctx context.Context, numFlows int, duration time.Duration) (*loadtest.FlowTestResult, error) {
	mft := &loadtest.ManyFlowTest{
		ServerAddr: "127.0.0.1:0",
		NumFlows:   numFlows,
		Duration:   duration,
		BurstSize:  10,
	}
	return mft.Run(ctx)
}

func runProfilingTests(ctx context.Context, binary, config, outputDir string, report *BenchmarkReport) error {
	pc := &profile.ProfilingConfig{
		BinaryPath:       binary,
		ConfigPath:       config,
		Duration:         5 * time.Second,
		ObfuscationModes: []string{"none", "standard"},
		OutputDir:        filepath.Join(outputDir, "profiles"),
	}

	var baseline, comparison *profile.ProfileResult
	var err error

	for _, mode := range pc.ObfuscationModes {
		fmt.Printf("  Profiling with obfuscation=%s...\n", mode)
		result, err := pc.RunProfile(ctx, mode)
		if err != nil {
			log.Printf("profiling failed: %v", err)
			continue
		}

		report.ProfileResults[mode] = result

		if mode == "none" {
			baseline = result
		} else if mode == "standard" {
			comparison = result
		}
	}

	if baseline != nil && comparison != nil {
		report.Comparison = &profile.ComparisonReport{
			BaselineMode:     baseline.Mode,
			BaselineResult:   baseline,
			ComparisonMode:   comparison.Mode,
			ComparisonResult: comparison,
		}
	}

	return err
}

func saveReport(outputDir string, report *BenchmarkReport) error {
	reportPath := filepath.Join(outputDir, "report.json")
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(reportPath, data, 0644)
}

func printSummary(report *BenchmarkReport) {
	fmt.Println("\n=== Benchmark Summary ===")

	if report.BulkTestResults != nil {
		fmt.Printf("\nBulk Transfer:\n")
		fmt.Printf("  Throughput: %.2f Gbps\n", report.BulkTestResults.Throughput)
		fmt.Printf("  Duration: %v\n", report.BulkTestResults.Duration)
		fmt.Printf("  Bytes Transferred: %d\n", report.BulkTestResults.BytesSent)
	}

	if report.FlowTestResults != nil {
		fmt.Printf("\nMany-Flow Test:\n")
		fmt.Printf("  Total Flows: %d\n", report.FlowTestResults.TotalFlows)
		fmt.Printf("  Successful: %d (%.1f%%)\n", report.FlowTestResults.SuccessFlows,
			float64(report.FlowTestResults.SuccessFlows)/float64(report.FlowTestResults.TotalFlows)*100)
		fmt.Printf("  Average Latency: %v\n", report.FlowTestResults.AvgLatency)
	}

	if report.Comparison != nil {
		comp := report.Comparison
		fmt.Printf("\nCPU Profiling Comparison:\n")
		fmt.Printf("  Baseline (%s): %d samples\n", comp.BaselineMode, comp.BaselineResult.Samples)
		fmt.Printf("  Comparison (%s): %d samples\n", comp.ComparisonMode, comp.ComparisonResult.Samples)
		if comp.BaselineResult.Samples > 0 {
			overhead := float64(comp.ComparisonResult.Samples-comp.BaselineResult.Samples) / float64(comp.BaselineResult.Samples) * 100
			fmt.Printf("  Overhead: %.1f%%\n", overhead)
		}
	}
}
