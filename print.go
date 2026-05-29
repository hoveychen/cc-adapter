package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/hoveychen/cc-adapter/internal/streamjson"
)

// printState drives the non-interactive `claude -p` output surface. It owns the
// host's RawSink so it can forward or capture the child's stream-json frames at
// full fidelity. The child always runs in webview stream-json mode; printState
// re-presents that stream to the downstream caller in the requested format.
type printState struct {
	format string    // "text" | "json" | "stream-json"
	out    io.Writer // downstream stdout (injectable for tests)

	mu         sync.Mutex
	resultLine []byte // raw "result" frame, captured for --output-format=json
}

// normalizeOutputFormat maps the downstream --output-format to a known value,
// defaulting to "text" (claude's own default for -p) for empty/unknown input.
func normalizeOutputFormat(f string) string {
	switch f {
	case "json", "stream-json":
		return f
	default:
		return "text"
	}
}

func newPrintState(outputFormat string) *printState {
	return &printState{format: normalizeOutputFormat(outputFormat), out: os.Stdout}
}

// sink receives every raw stdout line from the child (verbatim, with trailing
// newline). For stream-json it streams the frame straight through; for json it
// captures the terminal result frame; for text it does nothing (the result text
// is printed from the decoded result message).
func (p *printState) sink(line []byte) {
	switch p.format {
	case "stream-json":
		// Forward only the frames that are part of claude -p's output contract.
		// The control-channel frames (initialize/can_use_tool/mcp_message) are an
		// upstream VS Code transport artifact multiplexed on the same stdout; real
		// `claude -p --output-format stream-json` never emits them, and a
		// downstream consumer would choke on them.
		var t streamjson.TypeOnly
		if json.Unmarshal(line, &t) == nil && isControlFrame(t.Type) {
			return
		}
		p.out.Write(line)
	case "json":
		var t streamjson.TypeOnly
		if json.Unmarshal(line, &t) == nil && t.Type == streamjson.TypeResult {
			cp := make([]byte, len(line))
			copy(cp, line)
			p.mu.Lock()
			p.resultLine = cp
			p.mu.Unlock()
		}
	}
}

// isControlFrame reports whether a stream-json frame type belongs to the SDK
// control channel rather than claude's downstream output stream.
func isControlFrame(t string) bool {
	switch t {
	case streamjson.TypeControlRequest, streamjson.TypeControlResponse, streamjson.TypeControlCancelRequest:
		return true
	}
	return false
}

// runPrint executes one non-interactive turn: feed the prompt (from argv or
// stdin), wait for the result, and emit output in the requested format. It
// mirrors `claude -p`: text prints the final result text, json prints the
// result frame, stream-json has already been streamed by the sink.
func runPrint(host *streamjson.Host, opts cliOpts, ps *printState, turnDone <-chan *streamjson.ResultMessage, logger *log.Logger) error {
	if err := feedPrompt(host, opts, logger); err != nil {
		return err
	}
	// Do NOT close claude's stdin yet. Because we always inject in-process MCP
	// servers (ide + claude-vscode), claude initializes them with
	// MCP_CONNECTION_NONBLOCKING by sending mcp_message control_requests over the
	// control channel AFTER the user turn — and the answers travel back over
	// claude's stdin. Closing stdin here (before the turn produces a result)
	// severs that handshake: the mcp_response never lands, claude marks the
	// servers failed, and ide_tools drops to 0. Wait for the result first,
	// mirroring the relay path's firstResult discipline, then signal end-of-input
	// so claude exits.
	result := <-turnDone
	_ = host.CloseInput()

	switch ps.format {
	case "stream-json":
		// Frames already forwarded by the sink as they arrived; nothing to do.
	case "json":
		ps.mu.Lock()
		line := ps.resultLine
		ps.mu.Unlock()
		if len(line) > 0 {
			ps.out.Write(line)
		} else if result != nil {
			// Fallback: re-marshal the decoded result if the raw frame was missed.
			b, _ := json.Marshal(result)
			fmt.Fprintln(ps.out, string(b))
		}
	default: // text
		if result != nil {
			fmt.Fprintln(ps.out, result.Result)
		}
	}
	return nil
}

// feedPrompt sends the user input to the child. The prompt comes from the argv
// positional when present; otherwise it is read from stdin. With
// --input-format=stream-json the stdin lines are treated as pre-formed user
// turns; otherwise (text) the input is sent as a single plain-text turn.
func feedPrompt(host *streamjson.Host, opts cliOpts, logger *log.Logger) error {
	if p := opts.prompt(); p != "" {
		return host.SendUserText(p)
	}
	if opts.inputFormat == "stream-json" {
		return feedStreamJSON(host, os.Stdin, logger)
	}
	// text: whole stdin is one prompt.
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	return host.SendUserText(string(data))
}

// feedStreamJSON reads downstream stream-json user turns (one JSON object per
// line) and forwards each as a user turn to the child session.
func feedStreamJSON(host *streamjson.Host, r io.Reader, logger *log.Logger) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		text, ok := extractUserText(line)
		if !ok {
			logger.Printf("skipping non-user stream-json input line")
			continue
		}
		if err := host.SendUserText(text); err != nil {
			return err
		}
	}
	return sc.Err()
}

// extractUserText pulls the text content out of a downstream stream-json user
// message. It accepts both {message:{content:"..."}} and
// {message:{content:[{type:"text",text:"..."}]}} shapes.
func extractUserText(line []byte) (string, bool) {
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &env) != nil {
		return "", false
	}
	if env.Type != "" && env.Type != streamjson.TypeUser {
		return "", false
	}
	// content as a plain string.
	var s string
	if json.Unmarshal(env.Message.Content, &s) == nil {
		return s, true
	}
	// content as an array of blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(env.Message.Content, &blocks) == nil {
		var out string
		for _, b := range blocks {
			if b.Type == "text" {
				out += b.Text
			}
		}
		return out, true
	}
	return "", false
}

// isTerminal reports whether f is attached to a character device (a TTY) rather
// than a pipe or regular file.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
