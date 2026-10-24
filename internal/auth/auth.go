// Package auth manages the nixhook API key and the middleware that checks it.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const keyBytes = 32

// LoadOrCreate returns the key stored at path, generating one on first run.
func LoadOrCreate(path string) (key string, created bool, err error) {
	key, err = Load(path)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	key = generate()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if _, err := f.WriteString(key + "\n"); err != nil {
		return "", false, err
	}
	return key, true, nil
}

// Load reads the key stored at path.
func Load(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("key file %s is empty", path)
	}
	return key, nil
}

// Rotate replaces the key stored at path with a new one and returns it.
func Rotate(path string) (string, error) {
	key := generate()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(key+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return key, nil
}

// Match reports whether got equals want without leaking timing information.
func Match(want, got string) bool {
	if got == "" {
		return false
	}
	a := sha256.Sum256([]byte(want))
	b := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// Require wraps next so that it only runs for requests carrying the key at
// path as a bearer token. The key file is read per request so a rotation takes
// effect without restarting the server.
func Require(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := Load(path)
		if err != nil {
			slog.Error("load key", "err", err)
			http.Error(w, "server key unavailable", http.StatusInternalServerError)
			return
		}
		if !Match(key, bearer(r)) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

func generate() string {
	b := make([]byte, keyBytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}
