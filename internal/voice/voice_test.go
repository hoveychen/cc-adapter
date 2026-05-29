package voice

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestBuildDialURL_AllParams(t *testing.T) {
	got, err := buildDialURL(voiceStreamURL, Options{})
	if err != nil {
		t.Fatalf("buildDialURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if u.Scheme != "wss" || u.Host != "api.anthropic.com" {
		t.Errorf("scheme/host = %s://%s, want wss://api.anthropic.com", u.Scheme, u.Host)
	}
	if u.Path != "/api/ws/speech_to_text/voice_stream" {
		t.Errorf("path = %q", u.Path)
	}
	q := u.Query()
	want := map[string]string{
		"encoding":                "linear16",
		"sample_rate":             "16000",
		"channels":                "1",
		"endpointing_ms":          "300",
		"utterance_end_ms":        "1000",
		"language":                "en",
		"use_conversation_engine": "true",
		"stt_provider":            "deepgram-nova3",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("query[%s] = %q, want %q", k, q.Get(k), v)
		}
	}
	if q.Has("forward_interims") {
		t.Errorf("forward_interims present without ForwardInterims option")
	}
}

func TestBuildDialURL_LanguageAndInterims(t *testing.T) {
	got, err := buildDialURL(voiceStreamURL, Options{Language: "fr", ForwardInterims: true})
	if err != nil {
		t.Fatalf("buildDialURL: %v", err)
	}
	q, _ := url.Parse(got)
	if q.Query().Get("language") != "fr" {
		t.Errorf("language = %q, want fr", q.Query().Get("language"))
	}
	if q.Query().Get("forward_interims") != "typed" {
		t.Errorf("forward_interims = %q, want typed", q.Query().Get("forward_interims"))
	}
}

func TestBuildHeaders(t *testing.T) {
	h := buildHeaders("tok123", Options{ExtraKeyterms: []string{"Foo", "Bar"}})
	if h.Get("Authorization") != "Bearer tok123" {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if h.Get("x-app") != "vscode" {
		t.Errorf("x-app = %q, want vscode", h.Get("x-app"))
	}
	kt := h.Get("x-config-keyterms")
	// Default keyterms must be present, plus the extras appended.
	for _, want := range []string{"VS Code", "IDE", "OAuth", "Foo", "Bar"} {
		if !strings.Contains(kt, want) {
			t.Errorf("x-config-keyterms %q missing %q", kt, want)
		}
	}
	if !strings.HasSuffix(kt, "Foo,Bar") {
		t.Errorf("x-config-keyterms %q does not end with appended extras", kt)
	}
}

func TestKeytermsHeaderValue_NoExtras(t *testing.T) {
	got := keytermsHeaderValue(nil)
	if got != strings.Join(DefaultKeyterms, ",") {
		t.Errorf("keyterms = %q, want default joined", got)
	}
}

// TestConnectSendRecvClose spins up a fake gorilla WS server and verifies the
// full client protocol: handshake (headers reach the server), binary audio frame,
// KeepAlive text frame, CloseStream text frame, and reading a server reply.
// It never touches the real wss://api.anthropic.com endpoint.
func TestConnectSendRecvClose(t *testing.T) {
	type frame struct {
		typ  int
		data []byte
	}
	frames := make(chan frame, 8)
	gotHeaders := make(chan http.Header, 1)

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders <- r.Header.Clone()
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer c.Close()
		// Send one transcription-result reply so Recv has something to read.
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"Results","text":"hello"}`))
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			frames <- frame{mt, data}
			if mt == websocket.TextMessage && strings.Contains(string(data), "CloseStream") {
				return
			}
		}
	}))
	defer srv.Close()

	// Point the dialer at the fake ws:// server.
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws/speech_to_text/voice_stream"
	st, err := connect(wsBase, "faketoken", Options{ExtraKeyterms: []string{"Zed"}})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Verify the upgrade request carried the expected headers.
	h := <-gotHeaders
	if h.Get("x-app") != "vscode" {
		t.Errorf("server saw x-app = %q, want vscode", h.Get("x-app"))
	}
	if h.Get("Authorization") != "Bearer faketoken" {
		t.Errorf("server saw Authorization = %q", h.Get("Authorization"))
	}
	if !strings.Contains(h.Get("x-config-keyterms"), "Zed") {
		t.Errorf("server saw x-config-keyterms = %q, missing Zed", h.Get("x-config-keyterms"))
	}

	// Read the server's transcription reply.
	reply, err := st.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if !strings.Contains(string(reply), "hello") {
		t.Errorf("Recv = %q, want transcription reply", reply)
	}

	if err := st.SendAudio([]byte{0x01, 0x02, 0x03, 0x04}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	if err := st.KeepAlive(); err != nil {
		t.Fatalf("KeepAlive: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Assert the server received: binary audio, KeepAlive text, CloseStream text.
	f1 := <-frames
	if f1.typ != websocket.BinaryMessage || len(f1.data) != 4 {
		t.Errorf("frame1 type=%d len=%d, want binary len 4", f1.typ, len(f1.data))
	}
	f2 := <-frames
	if f2.typ != websocket.TextMessage || !strings.Contains(string(f2.data), "KeepAlive") {
		t.Errorf("frame2 = (%d,%q), want KeepAlive text", f2.typ, f2.data)
	}
	f3 := <-frames
	if f3.typ != websocket.TextMessage || !strings.Contains(string(f3.data), "CloseStream") {
		t.Errorf("frame3 = (%d,%q), want CloseStream text", f3.typ, f3.data)
	}
}

// TestConnectUsesAuthSeam verifies Connect() pulls the token via the authHeaders
// seam and strips the "Bearer " prefix correctly.
func TestConnectUsesAuthSeam(t *testing.T) {
	orig := authHeaders
	defer func() { authHeaders = orig }()
	authHeaders = func() (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer seamtoken"}, nil
	}
	tok, err := bearerToken()
	if err != nil {
		t.Fatalf("bearerToken: %v", err)
	}
	if tok != "seamtoken" {
		t.Errorf("token = %q, want seamtoken", tok)
	}
}
