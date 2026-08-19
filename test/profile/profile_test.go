package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestComparisonReportGeneration(t *testing.T) {
	baseline := &ProfileResult{
		Mode:       "none",
		CPUProfile: "/tmp/cpu_none.prof",
		Duration:   10,
		Samples:    1000,
	}

	comparison := &ProfileResult{
		Mode:       "standard",
		CPUProfile: "/tmp/cpu_standard.prof",
		Duration:   10,
		Samples:    1270,
	}

	report := &ComparisonReport{
		BaselineMode:     baseline.Mode,
		BaselineResult:   baseline,
		ComparisonMode:   comparison.Mode,
		ComparisonResult: comparison,
	}

	output := report.Generate()

	if len(output) == 0 {
		t.Errorf("report generation produced empty output")
	}

	expectedStrings := []string{
		"CPU Profiling Comparison Report",
		baseline.Mode,
		comparison.Mode,
		"Overhead",
	}

	for _, expected := range expectedStrings {
		if !contains(output, expected) {
			t.Errorf("report missing expected string: %s", expected)
		}
	}

	t.Logf("Generated report:\n%s", output)
}

func TestFormatFileSize(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.prof")

	if err := os.WriteFile(testFile, make([]byte, 2048), 0644); err != nil {
		t.Fatalf("create test file: %v", err)
	}

	size := formatFileSize(testFile)
	if size == "unknown" {
		t.Errorf("formatFileSize returned unknown")
	}

	if !contains(size, "KB") && !contains(size, "MB") && !contains(size, "B") {
		t.Errorf("formatFileSize returned unexpected format: %s", size)
	}

	t.Logf("File size formatted as: %s", size)
}

func TestFormatFileSizeNonexistent(t *testing.T) {
	size := formatFileSize("/nonexistent/file.prof")
	if size != "unknown" {
		t.Errorf("expected 'unknown' for nonexistent file, got %s", size)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
