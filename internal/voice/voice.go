// Package voice replicates the VS Code "Claude Code" extension's speech-to-text
// WebSocket (A5): the live microphone transcription stream the extension opens
// when the user dictates a prompt.
//
// Reverse-engineered from the shipped extension. Unlike the OAuth REST endpoints
// (cloud package), the speech stream connects to a FIXED production WebSocket
// regardless of ANTHROPIC_BASE_URL:
//
//	wss://api.anthropic.com/api/ws/speech_to_text/voice_stream?<query>
//
// query parameters (URLSearchParams in the extension):
//
//	encoding=linear16
//	sample_rate=16000
//	channels=1
//	endpointing_ms=300
//	utterance_end_ms=1000
//	language=en                       (or the configured language)
//	use_conversation_engine=true
//	stt_provider=deepgram-nova3
//	forward_interims=typed            (only when typed interims are enabled)
//
// headers:
//
//	Authorization: Bearer <oauthToken>
//	x-app: vscode
//	x-config-keyterms: <comma-separated keyterms>
//
// The keyterms list is a hardcoded constant in the extension (symbol f14) merged
// with any user-defined terms; the hardcoded set is replicated below.
//
// Protocol: once connected, the client streams PCM linear16 16kHz mono audio as
// binary WS messages, periodically sends {"type":"KeepAlive"} as a text JSON
// frame, and ends with {"type":"CloseStream"}. The server replies with
// transcription-result JSON frames.
package voice

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/hoveychen/cc-adapter/internal/auth"
)

// voiceStreamURL is the fixed production speech-to-text WebSocket base. It does
// NOT vary with ANTHROPIC_BASE_URL — the extension hardcodes api.anthropic.com
// for the voice stream.
const voiceStreamURL = "wss://api.anthropic.com/api/ws/speech_to_text/voice_stream"

// DefaultKeyterms is the extension's hardcoded keyterm set (symbol f14) sent in
// the x-config-keyterms header so the recognizer biases toward these domain
// terms. User-defined terms (Options.ExtraKeyterms) are appended to this set.
var DefaultKeyterms = []string{
	"VS Code", "IDE", "webview", "IntelliSense", "MCP", "symlink",
	"grep", "regex", "localhost", "codebase", "TypeScript", "JSON", "OAuth",
}

// Default protocol constants matching the extension's URLSearchParams.
const (
	defaultLanguage = "en"
	encoding        = "linear16"
	sampleRate      = "16000"
	channels        = "1"
	endpointingMS   = "300"
	utteranceEndMS  = "1000"
	sttProvider     = "deepgram-nova3"
)

// Options configures a voice Stream.
type Options struct {
	// Language is the BCP-47 language passed as the `language` query param.
	// Empty means "en".
	Language string
	// ExtraKeyterms are user-defined recognizer keyterms appended to
	// DefaultKeyterms in the x-config-keyterms header.
	ExtraKeyterms []string
	// ForwardInterims, when true, adds forward_interims=typed to the query so
	// the server forwards typed interim (non-final) transcripts.
	ForwardInterims bool
}

// authHeaders is the source of the OAuth headers (Authorization + anthropic-beta);
// a package var so tests can inject a fake token without touching the keychain.
var authHeaders = auth.AuthHeaders

// dialer is the WebSocket dialer used by Connect; overridable in tests so a
// httptest server (ws://) can be reached without hitting the real wss endpoint.
var dialer = websocket.DefaultDialer

// Stream wraps a live speech-to-text WebSocket connection.
type Stream struct {
	conn *websocket.Conn
}

// buildDialURL constructs the full ws/wss dial URL (base + query) for the given
// options. It is a pure function so the query construction is unit-testable.
func buildDialURL(base string, opts Options) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	lang := opts.Language
	if lang == "" {
		lang = defaultLanguage
	}
	q := url.Values{}
	q.Set("encoding", encoding)
	q.Set("sample_rate", sampleRate)
	q.Set("channels", channels)
	q.Set("endpointing_ms", endpointingMS)
	q.Set("utterance_end_ms", utteranceEndMS)
	q.Set("language", lang)
	q.Set("use_conversation_engine", "true")
	q.Set("stt_provider", sttProvider)
	if opts.ForwardInterims {
		q.Set("forward_interims", "typed")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// keytermsHeaderValue returns the comma-separated x-config-keyterms value:
// DefaultKeyterms followed by any user-supplied extra terms.
func keytermsHeaderValue(extra []string) string {
	terms := make([]string, 0, len(DefaultKeyterms)+len(extra))
	terms = append(terms, DefaultKeyterms...)
	terms = append(terms, extra...)
	return strings.Join(terms, ",")
}

// buildHeaders constructs the HTTP headers for the WebSocket upgrade request:
// the OAuth Authorization bearer, x-app: vscode, and x-config-keyterms. token is
// the raw OAuth access token (the "Bearer " prefix is added here). It is a pure
// function so header construction is unit-testable.
func buildHeaders(token string, opts Options) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("x-app", "vscode")
	h.Set("x-config-keyterms", keytermsHeaderValue(opts.ExtraKeyterms))
	return h
}

// bearerToken extracts the raw access token from the OAuth headers produced by
// authHeaders (which formats it as "Bearer <token>").
func bearerToken() (string, error) {
	hs, err := authHeaders()
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(hs["Authorization"], "Bearer "), nil
}

// Connect dials the speech-to-text WebSocket with the given options, attaching
// the OAuth bearer + x-app + x-config-keyterms headers, and returns a live
// Stream. The base URL defaults to the fixed production endpoint.
func Connect(opts Options) (*Stream, error) {
	token, err := bearerToken()
	if err != nil {
		return nil, err
	}
	return connect(voiceStreamURL, token, opts)
}

// connect is the testable core of Connect: it takes an explicit base URL and
// token so tests can point it at a httptest ws:// server with a fake token.
func connect(base, token string, opts Options) (*Stream, error) {
	dialURL, err := buildDialURL(base, opts)
	if err != nil {
		return nil, err
	}
	conn, _, err := dialer.Dial(dialURL, buildHeaders(token, opts))
	if err != nil {
		return nil, err
	}
	return &Stream{conn: conn}, nil
}

// SendAudio sends a frame of PCM linear16 16kHz mono audio as a binary WS message.
func (s *Stream) SendAudio(pcm []byte) error {
	return s.conn.WriteMessage(websocket.BinaryMessage, pcm)
}

// KeepAlive sends a {"type":"KeepAlive"} text JSON frame to keep the stream open
// during silence.
func (s *Stream) KeepAlive() error {
	return s.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"KeepAlive"}`))
}

// Recv reads one server message (a transcription-result JSON frame) and returns
// its payload.
func (s *Stream) Recv() ([]byte, error) {
	_, data, err := s.conn.ReadMessage()
	return data, err
}

// Close sends the {"type":"CloseStream"} terminator and closes the connection.
// The connection is always closed even if writing the terminator fails.
func (s *Stream) Close() error {
	werr := s.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"CloseStream"}`))
	cerr := s.conn.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
