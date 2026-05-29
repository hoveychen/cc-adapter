// Command cc-adapter drives the real `claude` binary exactly the way the VS Code
// "Claude Code" extension does in its default (webview) mode: it registers the
// IDE tools as an in-process SDK MCP server and spawns claude as a bidirectional
// stream-json child tagged CLAUDE_CODE_ENTRYPOINT=claude-vscode — so from
// Anthropic's backend the traffic is attributed to claude_code_vscode and the
// IDE tools (mcp__ide__*) are reachable, identical to a real VS Code session.
// The IDE JSON-RPC is tunneled over the stream-json control channel's
// mcp_message frames (not the terminal-mode WebSocket + lockfile + SSE_PORT).
//
// Upstream, claude ALWAYS runs as the realtime VS Code webview stream-json
// session (never one-shot `-p`): cc-adapter IS the stream-json host, owning the
// child's stdin/stdout, performing the initialize handshake, answering
// tool-permission requests over the stdio control channel, and reaching the IDE
// tools as an in-process MCP server. That is what keeps the upstream traffic
// indistinguishable from a real VS Code session.
//
// Downstream, cc-adapter presents a `claude -p`-compatible surface: it accepts
// -p, the full set of claude session flags (forwarded verbatim to the child),
// pipes, and --input-format / --output-format, then re-presents the child's
// stream-json frames to the caller in the requested format. The mismatch is the
// point — a cheap `claude -p` front bridged onto a full VS Code session.
//
// Usage:
//
//	cc-adapter "fix the bug in main.go"               # one-shot prompt, prints result, exits
//	cc-adapter                                        # interactive REPL (one stdin line per turn)
//	cc-adapter -p "summarize" --model claude-opus-4-8 # claude -p surface; --model forwarded to child
//	echo "summarize this" | cc-adapter -p             # prompt from a pipe
//	cc-adapter -p --output-format json "..."          # json / stream-json output like claude -p
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hoveychen/cc-adapter/internal/auth"
	"github.com/hoveychen/cc-adapter/internal/cloud"
	"github.com/hoveychen/cc-adapter/internal/ide"
	"github.com/hoveychen/cc-adapter/internal/streamjson"
	"github.com/hoveychen/cc-adapter/internal/telemetry"
	"github.com/hoveychen/cc-adapter/internal/voice"
)

// main delegates to run so that deferred cleanup actually executes — os.Exit,
// which we need for the child's exit code, skips deferred functions.
func main() {
	os.Exit(run())
}

func run() int {
	rawArgs := os.Args[1:]

	// Subcommand dispatch: when the first token is exactly one of the cloud
	// subcommands, hit the corresponding OAuth-authenticated endpoint, print its
	// result, and exit without starting the stream-json host. Any other first
	// token (a flag, a plain prompt, or none) falls through to the host path.
	if len(rawArgs) > 0 {
		switch rawArgs[0] {
		case "usage", "profile", "sessions":
			return runCloud(rawArgs[0])
		case "session", "teleport-events", "session-ingress":
			return runCloudWithID(rawArgs[0], rawArgs[1:])
		case "voice":
			return runVoice()
		}
	}

	// Version probe: the Claude Agent SDK runs `<cli> -v` (2s timeout) before
	// opening a session and parses a leading X.Y.Z from stdout. Real claude prints
	// its version and exits; cc-adapter must do the same or the SDK's preflight
	// hangs (a bare -v would otherwise be forwarded as a session flag and block on
	// stdin). Mirror claude by passing the version flag straight through to the
	// real binary and relaying its output. Matched anywhere in argv, as claude
	// treats -v/--version as print-version-and-exit.
	for _, a := range rawArgs {
		if a == "-v" || a == "--version" {
			return runVersion(a)
		}
	}

	opts := parseArgs(rawArgs)

	logger := log.New(os.Stderr, "[cc-adapter] ", log.LstdFlags)
	telemetry.SetLogger(logger)

	// accountUUID is best-effort: derived from the shared OAuth credentials when
	// available, empty otherwise (the event is still sent anonymously). Resolved
	// once up front so spawn-failure reporting doesn't touch the keychain on a
	// hot path.
	accountUUID := resolveAccountUUID()

	// emitTelemetry posts an internal event unless disabled by --no-telemetry.
	// The package itself also honours CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC /
	// DISABLE_TELEMETRY / DO_NOT_TRACK.
	emitTelemetry := func(name string, data map[string]any) {
		if opts.noTelemetry {
			return
		}
		telemetry.PostInternalEvent(name, data, accountUUID)
	}

	claudePath, err := resolveClaude(opts.claudeBin)
	if err != nil {
		logger.Printf("%v", err)
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cwd, _ := os.Getwd()
	var workspaceFolders []string
	if cwd != "" {
		workspaceFolders = []string{cwd}
	}

	// Register the IDE tools as an in-process MCP server. claude reaches them by
	// tunneling JSON-RPC over the stream-json control channel's mcp_message
	// frames (the default-webview-mode path).
	var mcpServer *ide.MCPServer
	if !opts.noIDE {
		provider := ide.NewHeadlessProvider(workspaceFolders)
		mcpServer = ide.NewMCPServer(provider, logger)
		logger.Printf("IDE in-process MCP server enabled (server name: ide)")
	}

	// Session-configuration flags pass through verbatim to the child claude. The
	// child always runs in webview stream-json mode regardless of the downstream
	// --output-format/--input-format the caller requested. dedupBaselineFlags
	// drops any forwarded copy of a flag the Host baseline already supplies (e.g.
	// the Agent SDK re-passing --verbose / --permission-prompt-tool stdio), so the
	// child never gets duplicates.
	extra := dedupBaselineFlags(opts.forward)

	// --include-partial-messages / --replay-user-messages are adapter-owned (we
	// consume them in parseArgs), but their effect is produced by the child: it
	// emits the extra stream_event / replayed-user frames, which the stream-json
	// sink then forwards downstream. So re-add them to the child's args when set.
	if opts.includePartial {
		extra = append(extra, "--include-partial-messages")
	}
	if opts.replayUserMessages {
		extra = append(extra, "--replay-user-messages")
	}

	// Relay mode: the downstream caller (the Claude Agent SDK) drives the full
	// bidirectional control protocol, so cc-adapter behaves as a faithful claude
	// child rather than the one-shot `claude -p` output emulation. The relay owns
	// stdin/stdout and bridges both control channels; the print/REPL paths below
	// are bypassed entirely.
	if opts.relayMode() {
		return runRelay(ctx, claudePath, mcpServer, extra, opts, logger, emitTelemetry)
	}

	perm := func(tool string, _ json.RawMessage) (bool, string) {
		if opts.denyWrites && isWriteTool(tool) {
			return false, "writes denied by --deny-writes"
		}
		return true, ""
	}

	// Mode: interactive REPL only when no -p, no positional prompt, and stdin is
	// a TTY. Otherwise (-p, a prompt, or a pipe/redirect) we run the
	// non-interactive print path that mirrors `claude -p`. The print path owns a
	// RawSink so it can forward/capture the child's stream-json frames verbatim.
	interactive := !opts.print && opts.prompt() == "" && isTerminal(os.Stdin)
	var ps *printState
	cfg := streamjson.Config{
		ClaudePath:    claudePath,
		MCPServer:     mcpServer,
		IDEServerName: "ide",
		ExtraArgs:     extra,
		Permission:    perm,
		Logger:        logger,
	}
	if !interactive {
		ps = newPrintState(opts.outputFormat)
		cfg.RawSink = ps.sink
	}

	host := streamjson.NewHost(cfg)
	if err := host.Start(ctx); err != nil {
		logger.Printf("%v", err)
		emitTelemetry("claude_spawn_failed", map[string]any{"phase": "spawn"})
		return 1
	}

	// Drain events. In interactive mode assistant text streams to stdout as it
	// arrives; in the print path the runPrint driver owns stdout, so the event
	// loop only routes results and diagnostics.
	turnDone := make(chan *streamjson.ResultMessage, 1)
	var launchedOnce sync.Once
	go func() {
		for ev := range host.Events {
			switch ev.Kind {
			case streamjson.EventSystemInit:
				ideTools := 0
				for _, t := range ev.System.Tools {
					if strings.HasPrefix(t, "mcp__ide__") {
						ideTools++
					}
				}
				logger.Printf("session=%s model=%s mcp_servers=%v ide_tools=%d (entrypoint=claude-vscode)",
					ev.System.SessionID, ev.System.Model, ev.System.MCPServers, ideTools)
				// Replicate the extension's per-session UI telemetry: push a
				// claude_launched log_event to the claude-vscode comm server. Run
				// in a goroutine so the event loop isn't blocked on the control
				// round-trip, and guard with Once so it fires exactly one time.
				launchedOnce.Do(func() {
					go func() {
						if err := host.SendLogEvent("claude_launched", map[string]any{"ide": "vscode", "isFullEditor": true}); err != nil {
							logger.Printf("log_event claude_launched: %v", err)
						}
					}()
				})
			case streamjson.EventAssistantText:
				if interactive {
					fmt.Println(ev.Text)
				}
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

	if interactive {
		runREPL(host, turnDone, logger)
	} else {
		if err := runPrint(host, opts, ps, turnDone, logger); err != nil {
			logger.Printf("print: %v", err)
			_ = host.CloseInput()
			return 1
		}
	}

	code := host.Wait()
	// A non-zero exit that we did not request via signal (ctx still live) means
	// claude died unexpectedly — the extension reports this as
	// claude_subprocess_exited_unexpectedly.
	if code != 0 && ctx.Err() == nil {
		emitTelemetry("claude_subprocess_exited_unexpectedly", map[string]any{"exit_code": code})
	}
	return code
}

// runCloud dispatches a cloud subcommand (usage / profile / sessions), prints
// the raw response body to stdout, and returns the process exit code. Errors are
// printed to stderr and yield exit code 1. It never starts the stream-json host.
func runCloud(cmd string) int {
	var (
		out []byte
		err error
	)
	switch cmd {
	case "usage":
		out, err = cloud.Usage()
	case "profile":
		_, out, err = cloud.Profile()
	case "sessions":
		out, err = cloud.RemoteSessions()
	default:
		fmt.Fprintf(os.Stderr, "cc-adapter: unknown cloud subcommand %q\n", cmd)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-adapter %s: %v\n", cmd, err)
		return 1
	}
	os.Stdout.Write(out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	return 0
}

// runCloudWithID dispatches a parameterized A6-derived cloud subcommand
// (session / teleport-events / session-ingress), each requiring a session id as
// rest[0]. It prints the raw response body to stdout and returns the exit code.
// A missing id prints usage to stderr and returns 1; errors print to stderr and
// return 1. It never starts the stream-json host.
func runCloudWithID(cmd string, rest []string) int {
	if len(rest) < 1 || rest[0] == "" {
		fmt.Fprintf(os.Stderr, "cc-adapter: %s requires a session id\nusage: cc-adapter %s <id>\n", cmd, cmd)
		return 1
	}
	id := rest[0]

	var (
		out []byte
		err error
	)
	switch cmd {
	case "session":
		out, err = cloud.SessionDetail(id)
	case "teleport-events":
		out, err = cloud.TeleportEvents(id)
	case "session-ingress":
		out, err = cloud.SessionIngress(id)
	default:
		fmt.Fprintf(os.Stderr, "cc-adapter: unknown cloud subcommand %q\n", cmd)
		return 1
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-adapter %s: %v\n", cmd, err)
		return 1
	}
	os.Stdout.Write(out)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		fmt.Println()
	}
	return 0
}

// runVoice replicates the VS Code extension's speech-to-text WebSocket (A5). It
// connects to the fixed production voice stream with the OAuth bearer, then:
//
//   - If stdin is a pipe carrying PCM linear16 16kHz mono audio, it streams the
//     audio as binary WS frames, sends a periodic KeepAlive, and on stdin EOF
//     sends CloseStream; transcription-result JSON frames from the server are
//     printed to stdout as they arrive.
//   - If stdin is a TTY (no piped audio), it only verifies the handshake: connect,
//     immediately Close, print a confirmation, and return 0.
//
// Errors go to stderr and yield exit code 1. It never starts the stream-json host.
func runVoice() int {
	lang := os.Getenv("CC_ADAPTER_VOICE_LANG")
	st, err := voice.Connect(voice.Options{Language: lang})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-adapter voice: connect: %v\n", err)
		return 1
	}

	// No piped audio (stdin is a terminal): verify handshake only.
	if fi, _ := os.Stdin.Stat(); fi != nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		_ = st.Close()
		fmt.Println("voice stream connected (no audio piped)")
		return 0
	}

	// Reader goroutine: print transcription-result frames to stdout until the
	// connection closes.
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			msg, err := st.Recv()
			if err != nil {
				return
			}
			os.Stdout.Write(msg)
			if len(msg) == 0 || msg[len(msg)-1] != '\n' {
				fmt.Println()
			}
		}
	}()

	// KeepAlive ticker keeps the stream open during silence.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := st.KeepAlive(); err != nil {
					return
				}
			}
		}
	}()

	// Stream PCM audio from stdin in fixed-size chunks (3200 bytes = 100ms of
	// linear16 16kHz mono).
	buf := make([]byte, 3200)
	var streamErr error
	for {
		n, rerr := os.Stdin.Read(buf)
		if n > 0 {
			if serr := st.SendAudio(buf[:n]); serr != nil {
				streamErr = serr
				break
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			streamErr = rerr
			break
		}
	}

	close(stop)
	if cerr := st.Close(); cerr != nil && streamErr == nil {
		streamErr = cerr
	}
	<-recvDone

	if streamErr != nil {
		fmt.Fprintf(os.Stderr, "cc-adapter voice: %v\n", streamErr)
		return 1
	}
	return 0
}

// runVersion answers the Agent SDK's `<cli> -v` preflight by passing the version
// flag straight to the real claude and relaying its stdout/stderr, so the SDK
// reads an authentic X.Y.Z and proceeds to open the real stream-json session.
// It resolves the real binary via -claude-bin / $CLAUDE_REAL_BIN / PATH and
// never starts the stream-json host.
func runVersion(versionFlag string) int {
	claudePath, err := resolveClaude("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-adapter %s: %v\n", versionFlag, err)
		return 1
	}
	cmd := exec.Command(claudePath, versionFlag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "cc-adapter %s: %v\n", versionFlag, err)
		return 1
	}
	return 0
}

// resolveAccountUUID derives the account UUID from the shared OAuth credentials,
// returning "" if credentials are unavailable or unparsable (telemetry is still
// sent anonymously in that case).
func resolveAccountUUID() string {
	creds, err := auth.Load()
	if err != nil {
		return ""
	}
	return auth.AccountUUID(creds.AccessToken)
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
