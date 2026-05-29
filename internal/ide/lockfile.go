package ide

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

// LockInfo mirrors the JSON the real VS Code extension writes to
// ~/.claude/ide/<port>.lock (reverse-engineered from extension.js, fn CJ):
//
//	{pid, workspaceFolders, ideName, transport:"ws", runningInWindows, authToken}
//
// The real claude binary reads this file to learn the authToken (and the IDE
// metadata); the port itself is delivered separately via CLAUDE_CODE_SSE_PORT.
type LockInfo struct {
	PID              int      `json:"pid"`
	WorkspaceFolders []string `json:"workspaceFolders"`
	IDEName          string   `json:"ideName"`
	Transport        string   `json:"transport"`
	RunningInWindows bool     `json:"runningInWindows"`
	AuthToken        string   `json:"authToken"`
}

// ClaudeConfigDir resolves the ~/.claude config dir, honouring CLAUDE_CONFIG_DIR
// exactly like the extension's I3() helper.
func ClaudeConfigDir() (string, error) {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// ideDir is <config>/ide, created with mode 0700 (extension gK0 uses mode 448).
func ideDir() (string, error) {
	cfg, err := ClaudeConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cfg, "ide")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// LockPath returns ~/.claude/ide/<port>.lock.
func LockPath(port int) (string, error) {
	dir, err := ideDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%d.lock", port)), nil
}

// WriteLock writes the lockfile with mode 0600 (extension uses mode 384).
func WriteLock(port int, info LockInfo) (string, error) {
	path, err := LockPath(port)
	if err != nil {
		return "", err
	}
	if info.Transport == "" {
		info.Transport = "ws"
	}
	info.RunningInWindows = runtime.GOOS == "windows"
	data, err := json.Marshal(info)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveLock deletes the lockfile, ignoring ENOENT.
func RemoveLock(port int) error {
	path, err := LockPath(port)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NewAuthToken returns a random UUIDv4 string, matching the extension's
// crypto.randomUUID() authToken.
func NewAuthToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// FindFreePort mirrors the extension's hK0/z84: pick a random port in
// [10000, 65535] and confirm it can be bound on 127.0.0.1; retry up to 50 times.
// It returns a listener already bound to the chosen port so there is no TOCTOU
// gap between the availability check and the server actually listening.
func FindFreePort() (net.Listener, int, error) {
	for i := 0; i < 50; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(55536))
		if err != nil {
			return nil, 0, err
		}
		port := int(n.Int64()) + 10000
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, fmt.Errorf("failed to find an available port after 50 attempts")
}
