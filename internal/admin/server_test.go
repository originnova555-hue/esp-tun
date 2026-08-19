package admin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdminServerAcceptsConnections(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(socketPath, nil)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
}

func TestAdminServerHelpCommand(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(socketPath, nil)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	fmt.Fprint(conn, "help\n")

	scanner := bufio.NewScanner(conn)
	var response strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		response.WriteString(line)
		if strings.Contains(line, "help") {
			break
		}
	}

	output := response.String()
	if !strings.Contains(output, "Commands:") {
		t.Fatalf("help output missing Commands header: %s", output)
	}
}

func TestAdminServerUnknownCommand(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(socketPath, nil)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	fmt.Fprint(conn, "nonexistent\n")

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no response from server")
	}

	output := scanner.Text()
	if !strings.Contains(output, "ERR") || !strings.Contains(output, "unknown command") {
		t.Fatalf("unexpected error response: %s", output)
	}
}

func TestAdminServerRegisterCustomCommand(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(socketPath, nil)
	defer srv.Close()

	srv.Register("custom", func(ctx context.Context, args []string, w io.Writer) error {
		fmt.Fprint(w, "CUSTOM_RESPONSE\n")
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	fmt.Fprint(conn, "custom\n")

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no response from server")
	}

	output := scanner.Text()
	if output != "CUSTOM_RESPONSE" {
		t.Fatalf("unexpected response: %s", output)
	}
}

func TestAdminServerCloseRemovesSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	srv := NewServer(socketPath, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("listen failed: %v", err)
	}

	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("socket file not created: %v", err)
	}

	srv.Close()

	if _, err := os.Stat(socketPath); err == nil {
		t.Fatalf("socket file still exists after close")
	}
}
