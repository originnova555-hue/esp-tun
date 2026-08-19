package profile

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

type ProfilingConfig struct {
	BinaryPath       string
	ConfigPath       string
	Duration         time.Duration
	ObfuscationModes []string
	OutputDir        string
}

type ProfileResult struct {
	Mode       string
	CPUProfile string
	Duration   time.Duration
	PeakMemory int64
	Samples    int
}

func (pc *ProfilingConfig) RunProfile(ctx context.Context, mode string) (*ProfileResult, error) {
	if err := os.MkdirAll(pc.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("create output dir: %w", err)
	}

	profileFile := filepath.Join(pc.OutputDir, fmt.Sprintf("cpu_%s.prof", mode))
	result := &ProfileResult{
		Mode:       mode,
		CPUProfile: profileFile,
		Duration:   pc.Duration,
	}

	tmpDir := filepath.Join(pc.OutputDir, "tmp_"+mode)
	os.MkdirAll(tmpDir, 0755)
	defer os.RemoveAll(tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := pc.patchConfig(cfgPath, mode); err != nil {
		return nil, fmt.Errorf("patch config: %w", err)
	}

	f, err := os.Create(profileFile)
	if err != nil {
		return nil, fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()

	if err := pprof.StartCPUProfile(f); err != nil {
		return nil, fmt.Errorf("start cpu profile: %w", err)
	}

	cmd := exec.CommandContext(ctx, pc.BinaryPath, "run", "-config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		if ctx.Err() == nil {
			return nil, fmt.Errorf("run command: %w", err)
		}
	}
	result.Duration = time.Since(start)

	pprof.StopCPUProfile()

	if samples, err := pc.countSamples(profileFile); err == nil {
		result.Samples = samples
	}

	return result, nil
}

func (pc *ProfilingConfig) patchConfig(path, mode string) error {
	content, err := os.ReadFile(pc.ConfigPath)
	if err != nil {
		return err
	}

	cfgStr := string(content)

	oldMode := "obfuscation"
	newMode := fmt.Sprintf(`[obfuscation]
mode = "%s"`, mode)

	if strings.Contains(cfgStr, "[obfuscation]") {
		parts := strings.Split(cfgStr, "[obfuscation]")
		if len(parts) > 1 {
			rest := parts[1]
			endIdx := strings.Index(rest, "\n[")
			if endIdx > 0 {
				rest = rest[endIdx:]
			}
			cfgStr = parts[0] + newMode + rest
		}
	} else {
		cfgStr += "\n" + newMode + "\n"
	}

	return os.WriteFile(path, []byte(cfgStr), 0644)
}

func (pc *ProfilingConfig) countSamples(profileFile string) (int, error) {
	cmd := exec.Command("go", "tool", "pprof", "-nodecount=1", profileFile)
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}

	lines := strings.Split(string(out), "\n")
	if len(lines) > 0 {
		parts := strings.Fields(lines[0])
		if len(parts) > 0 {
			if n, err := strconv.Atoi(parts[0]); err == nil {
				return n, nil
			}
		}
	}

	return 0, fmt.Errorf("could not parse sample count")
}

type ComparisonReport struct {
	BaselineMode    string
	BaselineResult  *ProfileResult
	ComparisonMode  string
	ComparisonResult *ProfileResult
	CPUOverhead     float64
	SamplesDiff     int
}

func (cr *ComparisonReport) Generate() string {
	baseline := cr.BaselineResult
	comparison := cr.ComparisonResult

	overhead := 0.0
	if baseline.Samples > 0 {
		overhead = float64(comparison.Samples-baseline.Samples) / float64(baseline.Samples) * 100
	}

	report := fmt.Sprintf(`CPU Profiling Comparison Report
===============================

Baseline Configuration: obfuscation.mode = "%s"
  CPU Profile: %s
  Duration: %v
  Total Samples: %d
  Profile Size: %s

Comparison Configuration: obfuscation.mode = "%s"
  CPU Profile: %s
  Duration: %v
  Total Samples: %d
  Profile Size: %s

Analysis:
---------
Sample Difference: %+d (%.2f%%)
CPU Overhead: %.2f%%

Interpretation:
  - Positive overhead means the comparison mode uses more CPU
  - Negative overhead means the comparison mode is more efficient
  - Baseline (none) represents the theoretical minimum CPU cost
  - Standard obfuscation adds padding and timing jitter overhead
`,
		baseline.Mode, baseline.CPUProfile, baseline.Duration, baseline.Samples, formatFileSize(baseline.CPUProfile),
		comparison.Mode, comparison.CPUProfile, comparison.Duration, comparison.Samples, formatFileSize(comparison.CPUProfile),
		comparison.Samples-baseline.Samples, float64(comparison.Samples-baseline.Samples)/float64(baseline.Samples)*100,
		overhead)

	return report
}

func formatFileSize(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown"
	}
	bytes := info.Size()
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.2f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/(1024*1024))
}
