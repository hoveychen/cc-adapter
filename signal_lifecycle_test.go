package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/hoveychen/cc-adapter/internal/ide"
	"github.com/hoveychen/cc-adapter/internal/streamjson"
)

// --- alternate fake-claude behaviours (dispatched from TestMain via CCA_FAKE_MODE) ---

func fakeEmitInit() {
	b, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "init", "session_id": "s1",
		"model": "fake", "tools": []string{}, "mcp_servers": []any{},
	})
	os.Stdout.Write(append(b, '\n'))
}

// fakeClaudeBlock emits system:init then blocks forever reading stdin. It never
// exits on its own; the test drives termination via ctx cancel / signals.
func fakeClaudeBlock() int {
	fakeEmitInit()
	io.Copy(io.Discard, os.Stdin) // blocks until stdin EOF (which the test never sends)
	return 0
}

// fakeClaudeDie emits system:init then exits cleanly a moment later WITHOUT
// reading further from stdin — simulating claude terminating for its own reason
// while the downstream SDK keeps cc-adapter's stdin open.
func fakeClaudeDie() int {
	fakeEmitInit()
	time.Sleep(150 * time.Millisecond)
	return 0
}

// fakeClaudeSignal mirrors the real claude's signal disposition observed in
// testing: SIGINT is trapped and exits 0 (graceful); SIGTERM is left at its
// default disposition so the process is killed by the signal (-> 143). It blocks
// until one of those arrives.
func fakeClaudeSignal() int {
	fakeEmitInit()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt) // trap SIGINT only; SIGTERM keeps default action
	<-ch
	return 0
}

// fakeClaudeWrapper writes a shell wrapper that re-execs this test binary in
// fake-claude mode with the given CCA_FAKE_MODE.
func fakeClaudeWrapper(t *testing.T, mode string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	wrapper := filepath.Join(t.TempDir(), "fake-claude.sh")
	script := "#!/bin/sh\nexport CCA_FAKE_CLAUDE=1\nexport CCA_FAKE_MODE=" + strconv.Quote(mode) +
		"\nexec " + strconv.Quote(self) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}
	return wrapper
}

// newRelayForTest builds a relay over a RelayMode Host wired exactly as runRelay
// does (RawSink -> r.onUpstreamLine), returning both so the test can Start the
// host and drive r.run.
func newRelayForTest(t *testing.T, wrapper string) (*streamjson.Host, *relay) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	mcpServer := ide.NewMCPServer(ide.NewHeadlessProvider(nil), logger)
	var r *relay
	host := streamjson.NewHost(streamjson.Config{
		ClaudePath:    wrapper,
		MCPServer:     mcpServer,
		IDEServerName: "ide",
		Logger:        logger,
		RelayMode:     true,
		RawSink:       func(line []byte) { r.onUpstreamLine(line) },
	})
	r = newRelay(host, false, logger)
	// Discard downstream output so the relay's writeDownstream never blocks.
	r.out = io.Discard
	return host, r
}

// TestRelay_FollowsClaudeDeath_WhenDownstreamStdinOpen reproduces bug #1: when
// claude exits for its own reason while the SDK keeps cc-adapter's stdin open,
// the relay must follow it down rather than hang on the open downstream reader.
func TestRelay_FollowsClaudeDeath_WhenDownstreamStdinOpen(t *testing.T) {
	wrapper := fakeClaudeWrapper(t, "die")
	host, r := newRelayForTest(t, wrapper)

	ctx := context.Background()
	if err := host.Start(ctx); err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	// A downstream stdin that never reaches EOF: io.Pipe whose write end we hold.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	done := make(chan int, 1)
	go func() { done <- r.run(ctx, pr) }()

	select {
	case <-done:
		// success: relay returned after claude died despite the open downstream stdin
	case <-time.After(5 * time.Second):
		t.Fatal("relay hung after claude exited (downstream stdin still open) — lifecycle not followed")
	}
}

// TestRelay_ExitsOnContextCancel reproduces bug #2: an OS signal to cc-adapter
// (modelled as ctx cancellation) must tear the relay down promptly even though
// the downstream stdin is still open.
func TestRelay_ExitsOnContextCancel(t *testing.T) {
	wrapper := fakeClaudeWrapper(t, "block")
	host, r := newRelayForTest(t, wrapper)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := host.Start(ctx); err != nil {
		t.Fatalf("host.Start: %v", err)
	}

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	done := make(chan int, 1)
	go func() { done <- r.run(ctx, pr) }()

	time.Sleep(500 * time.Millisecond) // let the child boot
	cancel()

	select {
	case <-done:
		// success: relay returned on ctx cancel
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not exit on ctx cancel — hung on open downstream stdin")
	}
}

// TestHost_ForwardsSignalAndMapsExitCode reproduces bug #3: on ctx cancel the
// Host must deliver ForwardSignal()'s signal (not an unconditional SIGKILL) to
// claude, and Wait must map a signal-killed child to 128+signum — so the exit
// code matches real claude (SIGINT->0 via its own trap, SIGTERM->143).
func TestHost_ForwardsSignalAndMapsExitCode(t *testing.T) {
	cases := []struct {
		name string
		sig  os.Signal
		want int
	}{
		{"SIGINT_graceful_zero", syscall.SIGINT, 0},
		{"SIGTERM_maps_to_143", syscall.SIGTERM, 143},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapper := fakeClaudeWrapper(t, "signal")
			logger := log.New(io.Discard, "", 0)
			host := streamjson.NewHost(streamjson.Config{
				ClaudePath:    wrapper,
				IDEServerName: "ide",
				Logger:        logger,
				ForwardSignal: func() os.Signal { return tc.sig },
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := host.Start(ctx); err != nil {
				t.Fatalf("host.Start: %v", err)
			}
			go func() {
				for range host.Events {
				}
			}()
			time.Sleep(500 * time.Millisecond) // let the child install its handler
			cancel()

			codeCh := make(chan int, 1)
			go func() { codeCh <- host.Wait() }()
			select {
			case got := <-codeCh:
				if got != tc.want {
					t.Fatalf("exit code = %d, want %d", got, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("host.Wait did not return after ctx cancel")
			}
		})
	}
}
