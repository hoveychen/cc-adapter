// Command cc-adapter drives the real `claude` binary exactly the way the VS Code
// "Claude Code" extension does: it spins up the IDE WebSocket side-channel
// (lockfile + MCP server), then spawns claude as a bidirectional stream-json
// child tagged CLAUDE_CODE_ENTRYPOINT=claude-vscode — so from Anthropic's
// backend the traffic is attributed to claude_code_vscode and the IDE
// auto-connect path is exercised, identical to a real VS Code session.
//
// Unlike a transparent passthrough wrapper (which inherits whatever mode the
// user invokes), cc-adapter IS the stream-json host: it owns stdin/stdout,
// performs the initialize handshake, answers tool-permission requests over the
// stdio control channel, and renders assistant output. claude never runs in
// one-shot `-p` mode here; it runs the realtime streaming session VS Code uses.
//
// Usage:
//
//	cc-adapter "fix the bug in main.go"     # one-shot prompt, prints result, exits
//	cc-adapter                              # interactive REPL (one stdin line per turn)
//	cc-adapter -model claude-opus-4-8 ...   # pass flags through to claude
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hoveychen/cc-adapter/internal/ide"
	"github.com/hoveychen/cc-adapter/internal/streamjson"
)

func main() {
	var (
		model      = flag.String("model", "", "pass --model to claude")
		noIDE      = flag.Bool("no-ide", false, "do not start the IDE side-channel server (billing attribution still applies)")
		denyWrites = flag.Bool("deny-writes", false, "deny write-class tools (Write/Edit/MultiEdit/NotebookEdit/Bash)")
		claudeBin  = flag.String("claude-bin", "", "path to the real claude binary (else $CLAUDE_REAL_BIN, else PATH)")
	)
	flag.Parse()
	prompt := strings.Join(flag.Args(), " ")

	logger := log.New(os.Stderr, "[cc-adapter] ", log.LstdFlags)

	claudePath, err := resolveClaude(*claudeBin)
	if err != nil {
		logger.Fatalf("%v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cwd, _ := os.Getwd()
	var workspaceFolders []string
	if cwd != "" {
		workspaceFolders = []string{cwd}
	}

	// Bring up the IDE side-channel so claude dials back in (the VS Code path).
	var port int
	if !*noIDE {
		provider := ide.NewHeadlessProvider(workspaceFolders)
		srv, err := ide.NewServer(provider, logger)
		if err != nil {
			logger.Fatalf("ide server: %v", err)
		}
		srv.Start()
		defer srv.Stop()

		lock := ide.LockInfo{
			PID:              os.Getpid(),
			WorkspaceFolders: workspaceFolders,
			IDEName:          "Visual Studio Code",
			Transport:        "ws",
			AuthToken:        srv.AuthToken(),
		}
		lockPath, err := ide.WriteLock(srv.Port(), lock)
		if err != nil {
			logger.Fatalf("lockfile: %v", err)
		}
		defer func() { _ = ide.RemoveLock(srv.Port()) }()
		logger.Printf("IDE side-channel on 127.0.0.1:%d, lock %s", srv.Port(), lockPath)
		port = srv.Port()
	}

	var extra []string
	if *model != "" {
		extra = append(extra, "--model", *model)
	}

	perm := func(tool string, _ json.RawMessage) (bool, string) {
		if *denyWrites && isWriteTool(tool) {
			return false, "writes denied by --deny-writes"
		}
		return true, ""
	}

	host := streamjson.NewHost(streamjson.Config{
		ClaudePath: claudePath,
		Port:       port,
		ExtraArgs:  extra,
		Permission: perm,
		Logger:     logger,
	})
	if err := host.Start(ctx); err != nil {
		logger.Fatalf("%v", err)
	}

	// Drain events: print assistant text to stdout, everything else to stderr.
	// A nil on turnDone means the stream closed (claude exited).
	turnDone := make(chan *streamjson.ResultMessage, 1)
	go func() {
		for ev := range host.Events {
			switch ev.Kind {
			case streamjson.EventSystemInit:
				logger.Printf("session=%s model=%s (entrypoint=claude-vscode)", ev.System.SessionID, ev.System.Model)
			case streamjson.EventAssistantText:
				fmt.Println(ev.Text)
			case streamjson.EventToolUse:
				logger.Printf("[tool] %s %s", ev.ToolName, string(ev.ToolInput))
			case streamjson.EventResult:
				select {
				case turnDone <- ev.Result:
				default:
				}
			case streamjson.EventError:
				logger.Printf("error: %s", ev.Text)
			}
		}
		select {
		case turnDone <- nil:
		default:
		}
	}()

	if err := host.Initialize(); err != nil {
		logger.Printf("initialize: %v", err)
	}

	if prompt != "" {
		if err := host.SendUserText(prompt); err != nil {
			logger.Fatalf("send: %v", err)
		}
		<-turnDone
		_ = host.CloseInput()
	} else {
		runREPL(host, turnDone, logger)
	}
	os.Exit(host.Wait())
}

// runREPL reads one user turn per stdin line until EOF or /exit.
func runREPL(host *streamjson.Host, turnDone <-chan *streamjson.ResultMessage, logger *log.Logger) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for {
		fmt.Fprint(os.Stderr, "\n> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		if err := host.SendUserText(line); err != nil {
			logger.Printf("send: %v", err)
			break
		}
		if r := <-turnDone; r == nil {
			break // stream closed
		}
	}
	_ = host.CloseInput()
}

// resolveClaude locates the real claude binary. It must not resolve to this
// adapter; when cc-adapter is installed as "claude" on PATH, set CLAUDE_REAL_BIN
// or -claude-bin. A good default is the binary the VS Code extension ships at
// ~/.vscode/extensions/anthropic.claude-code-*/resources/native-binary/claude.
func resolveClaude(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if p := os.Getenv("CLAUDE_REAL_BIN"); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("real claude binary not found: set -claude-bin, $CLAUDE_REAL_BIN, or put claude on PATH")
	}
	return p, nil
}

func isWriteTool(name string) bool {
	switch name {
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "Bash":
		return true
	}
	return false
}
