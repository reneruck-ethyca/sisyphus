package lastmile

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// TokenFile is where the handoff API's token lives, next to lastmile.json.
func TokenFile(stateDir string) string { return filepath.Join(stateDir, "lastmile.token") }

// loadToken reads the token, creating it (0600) on first use. A handoff can
// carry a --notify shell command, so creating one needs more than reaching the
// loopback port: review containers may reach it too, but don't mount the state dir.
func loadToken(stateDir string) (string, error) {
	path := TokenFile(stateDir)
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", err
	}
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// Authorized says whether tok is the handoff API's token.
func (m *Manager) Authorized(tok string) bool {
	return m.token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(m.token)) == 1
}
