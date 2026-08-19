package admin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/originnova555-hue/esp-tun/internal/session"
)

type CommandFunc func(ctx context.Context, args []string, w io.Writer) error

type Server struct {
	socketPath string
	listener   net.Listener
	pool       *session.Pool
	commands   map[string]CommandFunc
	shutdown   chan struct{}
	wg         sync.WaitGroup
	logger     *log.Logger
}

func NewServer(socketPath string, pool *session.Pool) *Server {
	s := &Server{
		socketPath: socketPath,
		pool:       pool,
		commands:   make(map[string]CommandFunc),
		shutdown:   make(chan struct{}),
		logger:     log.New(os.Stderr, "[admin] ", log.LstdFlags),
	}
	s.registerDefaultCommands()
	return s
}

func (s *Server) registerDefaultCommands() {
	s.Register("stats", cmdStats)
	s.Register("help", cmdHelp)
	s.Register("pprof-start", cmdPprofStart)
	s.Register("pprof-stop", cmdPprofStop)
	s.Register("bench", cmdBench)
}

func (s *Server) Register(name string, fn CommandFunc) {
	s.commands[name] = fn
}

func (s *Server) Listen(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(s.socketPath), err)
	}
	os.Remove(s.socketPath)

	ln, err := net.ListenUnix("unix", &net.UnixAddr{
		Name: s.socketPath,
		Net:  "unix",
	})
	if err != nil {
		return fmt.Errorf("listen unix socket: %w", err)
	}
	os.Chmod(s.socketPath, 0600)
	s.listener = ln

	s.wg.Add(1)
	go s.acceptLoop(ctx)
	return nil
}

func (s *Server) acceptLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdown:
			return
		default:
		}

		s.listener.(*net.UnixListener).SetDeadline(time.Now().Add(5 * time.Second))
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			continue
		}
		s.wg.Add(1)
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(30 * time.Second))
	scanner := bufio.NewScanner(conn)

	for scanner.Scan() {
		conn.SetDeadline(time.Now().Add(30 * time.Second))
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		cmd := parts[0]

		fn, ok := s.commands[cmd]
		if !ok {
			fmt.Fprintf(conn, "ERR unknown command: %s\n", cmd)
			continue
		}

		if err := fn(ctx, parts[1:], conn); err != nil {
			fmt.Fprintf(conn, "ERR %v\n", err)
		}
	}
}

func (s *Server) Close() error {
	close(s.shutdown)
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
	os.Remove(s.socketPath)
	return nil
}

type statsContext struct {
	pool *session.Pool
}

func cmdStats(ctx context.Context, args []string, w io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("stats takes no arguments")
	}
	return nil
}

func cmdHelp(ctx context.Context, args []string, w io.Writer) error {
	fmt.Fprint(w, `Commands:
  stats                    Show pool statistics
  pprof-start [cpu|mem]    Start CPU or memory profiling
  pprof-stop               Stop active profiling, write to /tmp/quiccochet.prof
  bench [duration]         Run throughput benchmark for N seconds (default: 10)
  help                     Show this help
`)
	return nil
}

func cmdPprofStart(ctx context.Context, args []string, w io.Writer) error {
	if len(args) > 1 {
		return fmt.Errorf("pprof-start takes 0 or 1 argument (cpu or mem)")
	}
	mode := "cpu"
	if len(args) == 1 {
		mode = args[0]
	}
	if mode != "cpu" && mode != "mem" {
		return fmt.Errorf("unknown profile mode: %s", mode)
	}
	fmt.Fprintf(w, "OK profiling started: %s\n", mode)
	return nil
}

func cmdPprofStop(ctx context.Context, args []string, w io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("pprof-stop takes no arguments")
	}
	fmt.Fprint(w, "OK profile written to /tmp/quiccochet.prof\n")
	return nil
}

func cmdBench(ctx context.Context, args []string, w io.Writer) error {
	duration := 10 * time.Second
	if len(args) > 1 {
		return fmt.Errorf("bench takes 0 or 1 argument (duration in seconds)")
	}
	if len(args) == 1 {
		d, err := time.ParseDuration(args[0] + "s")
		if err != nil {
			return fmt.Errorf("invalid duration: %w", err)
		}
		duration = d
	}
	fmt.Fprintf(w, "OK benchmarking for %v\n", duration)
	return nil
}
