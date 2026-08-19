package loadtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type BulkTestResult struct {
	Throughput float64
	Duration   time.Duration
	BytesSent  int64
	Errors     int
}

type FlowTestResult struct {
	TotalFlows   int
	SuccessFlows int
	FailedFlows  int
	AvgLatency   time.Duration
	Errors       int
	Duration     time.Duration
}

type BulkTest struct {
	ServerAddr string
	Duration   time.Duration
	BufferSize int
}

func (bt *BulkTest) Run(ctx context.Context) (*BulkTestResult, error) {
	ln, err := net.Listen("tcp", bt.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	serverAddr := ln.Addr().String()
	var wg sync.WaitGroup
	var result BulkTestResult
	var mu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		bt.runServer(ctx, ln, &result, &mu)
	}()

	time.Sleep(100 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		bt.runClient(ctx, serverAddr, &result, &mu)
	}()

	wg.Wait()
	return &result, nil
}

func (bt *BulkTest) runServer(ctx context.Context, ln net.Listener, result *BulkTestResult, mu *sync.Mutex) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ln.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}

		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, bt.BufferSize)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				c.SetDeadline(time.Now().Add(5 * time.Second))
				n, err := c.Read(buf)
				if err != nil {
					return
				}

				mu.Lock()
				result.BytesSent += int64(n)
				mu.Unlock()
			}
		}(conn)
	}
}

func (bt *BulkTest) runClient(ctx context.Context, serverAddr string, result *BulkTestResult, mu *sync.Mutex) {
	conn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		mu.Lock()
		result.Errors++
		mu.Unlock()
		return
	}
	defer conn.Close()

	buf := make([]byte, bt.BufferSize)
	for i := range buf {
		buf[i] = byte(i % 256)
	}

	start := time.Now()
	deadline := start.Add(bt.Duration)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if time.Now().After(deadline) {
			break
		}

		conn.SetDeadline(deadline)
		_, err := conn.Write(buf)
		if err != nil {
			mu.Lock()
			result.Errors++
			mu.Unlock()
			return
		}
	}

	elapsed := time.Since(start)
	mu.Lock()
	result.Duration = elapsed
	result.Throughput = float64(result.BytesSent) * 8 / elapsed.Seconds() / 1e9
	mu.Unlock()
}

type ManyFlowTest struct {
	ServerAddr  string
	NumFlows    int
	Duration    time.Duration
	BurstSize   int
}

func (mft *ManyFlowTest) Run(ctx context.Context) (*FlowTestResult, error) {
	ln, err := net.Listen("tcp", mft.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	serverAddr := ln.Addr().String()
	result := &FlowTestResult{TotalFlows: mft.NumFlows}
	var mu sync.Mutex

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		mft.runServer(ctx, ln, result, &mu)
	}()

	time.Sleep(100 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		mft.runClients(ctx, serverAddr, result, &mu)
	}()

	wg.Wait()
	return result, nil
}

func (mft *ManyFlowTest) runServer(ctx context.Context, ln net.Listener, result *FlowTestResult, mu *sync.Mutex) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ln.(*net.TCPListener).SetDeadline(time.Now().Add(1 * time.Second))
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}

		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 1024)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}

				c.SetDeadline(time.Now().Add(5 * time.Second))
				_, err := c.Read(buf)
				if err != nil {
					return
				}
			}
		}(conn)
	}
}

func (mft *ManyFlowTest) runClients(ctx context.Context, serverAddr string, result *FlowTestResult, mu *sync.Mutex) {
	deadline := time.Now().Add(mft.Duration)
	var wg sync.WaitGroup
	var semaphore = make(chan struct{}, mft.BurstSize)

	for i := 0; i < mft.NumFlows; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if time.Now().After(deadline) {
			break
		}

		semaphore <- struct{}{}
		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()

			start := time.Now()
			conn, err := net.Dial("tcp", serverAddr)
			if err != nil {
				mu.Lock()
				result.FailedFlows++
				result.Errors++
				mu.Unlock()
				return
			}
			defer conn.Close()

			conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, err = conn.Write([]byte("ping"))
			if err != nil {
				mu.Lock()
				result.FailedFlows++
				result.Errors++
				mu.Unlock()
				return
			}

			buf := make([]byte, 4)
			_, err = conn.Read(buf)
			if err != nil {
				mu.Lock()
				result.FailedFlows++
				result.Errors++
				mu.Unlock()
				return
			}

			latency := time.Since(start)
			mu.Lock()
			result.SuccessFlows++
			result.AvgLatency = (result.AvgLatency + latency) / 2
			mu.Unlock()
		}()
	}

	wg.Wait()
	result.Duration = time.Since(time.Now().Add(-mft.Duration))
}

func RunIperf3Server(ctx context.Context, addr, port string) error {
	cmd := exec.CommandContext(ctx, "iperf3", "-s", "-B", addr, "-p", port)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func RunIperf3Client(ctx context.Context, serverAddr string, port string, duration time.Duration) (string, error) {
	args := []string{
		"-c", serverAddr,
		"-p", port,
		"-t", fmt.Sprintf("%d", int(duration.Seconds())),
		"-J",
	}
	cmd := exec.CommandContext(ctx, "iperf3", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func ParseIperf3JSON(output string) (float64, error) {
	if !strings.Contains(output, "bits_per_second") {
		return 0, fmt.Errorf("no bandwidth info in output")
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "bits_per_second") {
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				bpsStr := strings.TrimSpace(strings.TrimSuffix(parts[1], ","))
				var bps float64
				if _, err := fmt.Sscanf(bpsStr, "%f", &bps); err == nil {
					return bps / 1e9, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("could not parse bandwidth from output")
}
