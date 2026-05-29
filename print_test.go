package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestNormalizeOutputFormat(t *testing.T) {
	cases := map[string]string{
		"":            "text",
		"text":        "text",
		"json":        "json",
		"stream-json": "stream-json",
		"bogus":       "text",
	}
	for in, want := range cases {
		if got := normalizeOutputFormat(in); got != want {
			t.Errorf("normalizeOutputFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsControlFrame(t *testing.T) {
	control := []string{"control_request", "control_response", "control_cancel_request"}
	for _, c := range control {
		if !isControlFrame(c) {
			t.Errorf("isControlFrame(%q) = false, want true", c)
		}
	}
	output := []string{"system", "assistant", "user", "result", "rate_limit_event", "stream_event"}
	for _, o := range output {
		if isControlFrame(o) {
			t.Errorf("isControlFrame(%q) = true, want false", o)
		}
	}
}

func TestExtractUserText_StringContent(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"hello there"}}`)
	got, ok := extractUserText(line)
	if !ok || got != "hello there" {
		t.Fatalf("extractUserText = (%q, %v)", got, ok)
	}
}

func TestExtractUserText_BlockContent(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`)
	got, ok := extractUserText(line)
	if !ok || got != "ab" {
		t.Fatalf("extractUserText = (%q, %v)", got, ok)
	}
}

func TestExtractUserText_NoTypeDefaultsToUser(t *testing.T) {
	// A bare {message:{content}} with no type is accepted as a user turn.
	line := []byte(`{"message":{"content":"x"}}`)
	got, ok := extractUserText(line)
	if !ok || got != "x" {
		t.Fatalf("extractUserText = (%q, %v)", got, ok)
	}
}

func TestExtractUserText_NonUserSkipped(t *testing.T) {
	line := []byte(`{"type":"result","result":"done"}`)
	if _, ok := extractUserText(line); ok {
		t.Fatal("non-user frame should be skipped")
	}
}

func TestSink_StreamJSONFiltersControlFrames(t *testing.T) {
	var buf bytes.Buffer
	ps := &printState{format: "stream-json", out: &buf}
	frames := []string{
		`{"type":"control_response","response":{}}`,
		`{"type":"system","subtype":"init"}`,
		`{"type":"control_request","request":{}}`,
		`{"type":"assistant","message":{"role":"assistant"}}`,
		`{"type":"result","subtype":"success"}`,
	}
	for _, f := range frames {
		ps.sink([]byte(f + "\n"))
	}
	out := buf.String()
	if strings.Contains(out, "control_request") || strings.Contains(out, "control_response") {
		t.Fatalf("control frames leaked into stream-json output:\n%s", out)
	}
	for _, want := range []string{`"type":"system"`, `"type":"assistant"`, `"type":"result"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %s:\n%s", want, out)
		}
	}
}

func TestSink_JSONCapturesResultFrame(t *testing.T) {
	var buf bytes.Buffer // json sink does not stream; it captures
	ps := &printState{format: "json", out: &buf}
	ps.sink([]byte(`{"type":"system","subtype":"init"}` + "\n"))
	ps.sink([]byte(`{"type":"assistant","message":{}}` + "\n"))
	resultFrame := `{"type":"result","subtype":"success","result":"hi"}` + "\n"
	ps.sink([]byte(resultFrame))

	if buf.Len() != 0 {
		t.Fatalf("json sink should not stream intermediate frames, got: %s", buf.String())
	}
	ps.mu.Lock()
	captured := string(ps.resultLine)
	ps.mu.Unlock()
	if captured != resultFrame {
		t.Fatalf("captured result = %q, want %q", captured, resultFrame)
	}
}
