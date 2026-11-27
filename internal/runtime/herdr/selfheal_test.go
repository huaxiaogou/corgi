package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallRawRetriesOnlyIdempotentReads(t *testing.T) {
	origDelay := herdrReadRetryDelay
	herdrReadRetryDelay = 5 * time.Millisecond
	defer func() { herdrReadRetryDelay = origDelay }()

	// 1. Idempotent read (agent.list) drops EOF on first attempt, succeeds on retry.
	var readCalls atomic.Int32
	readClient := NewClient(func(ctx context.Context) (net.Conn, error) {
		n := readCalls.Add(1)
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			rd := bufio.NewReader(c2)
			line, err := rd.ReadBytes('\n')
			if err != nil {
				return
			}
			if n == 1 {
				// Simulate mid-flight daemon restart EOF before replying.
				return
			}
			var req struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(line, &req)
			resp, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"agents": []any{}}})
			_, _ = c2.Write(append(resp, '\n'))
		}()
		return c1, nil
	})
	if _, err := readClient.Agents(context.Background()); err != nil {
		t.Fatalf("expected idempotent agent.list to recover on retry, got %v", err)
	}
	if got := readCalls.Load(); got != 2 {
		t.Fatalf("expected exactly 2 attempts for agent.list, got %d", got)
	}

	// 2. Mutating RPC (pane.send_keys) MUST NEVER retry on mid-flight EOF.
	var writeCalls atomic.Int32
	writeClient := NewClient(func(ctx context.Context) (net.Conn, error) {
		writeCalls.Add(1)
		c1, c2 := net.Pipe()
		go func() {
			defer c2.Close()
			_, _ = io.CopyN(io.Discard, c2, 1)
		}()
		return c1, nil
	})
	if err := writeClient.SendKeys(context.Background(), "w1:p1", "enter"); err == nil {
		t.Fatal("expected mutating pane.send_keys EOF to return error without retry")
	}
	if got := writeCalls.Load(); got != 1 {
		t.Fatalf("mutating RPC must never retry: got %d attempts", got)
	}
}

func TestDialUnixSharesBootWindowAndReconnectGrace(t *testing.T) {
	origBoot := herdrBootWait
	origReconn := herdrReconnectWait
	origCooldown := herdrStartCooldown
	origPoll := herdrPollInterval
	origStart := herdrStartDaemonFn
	defer func() {
		herdrBootWait = origBoot
		herdrReconnectWait = origReconn
		herdrStartCooldown = origCooldown
		herdrPollInterval = origPoll
		herdrStartDaemonFn = origStart
	}()

	herdrBootWait = 800 * time.Millisecond
	herdrReconnectWait = 500 * time.Millisecond
	herdrStartCooldown = 30 * time.Second
	herdrPollInterval = 15 * time.Millisecond

	tmpDir, err := os.MkdirTemp("/tmp", "corgi-herdr-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	sock := filepath.Join(tmpDir, "h.sock")

	var starts atomic.Int32
	var lnMu sync.Mutex
	var ln net.Listener
	startListenerAfter := func(delay time.Duration) {
		go func() {
			time.Sleep(delay)
			l, lErr := net.Listen("unix", sock)
			if lErr != nil {
				return
			}
			lnMu.Lock()
			ln = l
			lnMu.Unlock()
			for {
				c, aErr := l.Accept()
				if aErr != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}

	herdrStartDaemonFn = func(_ string, _ *slog.Logger) bool {
		starts.Add(1)
		startListenerAfter(70 * time.Millisecond)
		return true
	}

	// 1. Concurrent callers must share one daemon start + boot window and both succeed.
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := DialUnix(context.Background(), sock, tmpDir, nil)
			if err != nil {
				errs <- err
				return
			}
			_ = c.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent DialUnix failed: %v", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 shared daemon start, got %d", got)
	}

	// 2. Simulate brief daemon restart after previous connection: should reconnect in grace window without spawning another start.
	lnMu.Lock()
	if ln != nil {
		_ = ln.Close()
	}
	lnMu.Unlock()
	_ = os.Remove(sock)
	startListenerAfter(60 * time.Millisecond)

	c2, err := DialUnix(context.Background(), sock, tmpDir, nil)
	if err != nil {
		t.Fatalf("expected reconnect within grace window, got %v", err)
	}
	_ = c2.Close()
	if got := starts.Load(); got != 1 {
		t.Fatalf("expected reconnect grace window to avoid spawning new daemon, starts=%d", got)
	}

	lnMu.Lock()
	if ln != nil {
		_ = ln.Close()
	}
	lnMu.Unlock()
}
