package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "key")

	key, created, err := LoadOrCreate(path)
	if err != nil || !created || len(key) != keyBytes*2 {
		t.Fatalf("first call: key=%q created=%v err=%v", key, created, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}

	again, created, err := LoadOrCreate(path)
	if err != nil || created || again != key {
		t.Fatalf("second call: key=%q created=%v err=%v", again, created, err)
	}

	rotated, err := Rotate(path)
	if err != nil || rotated == key {
		t.Fatalf("rotate: key=%q err=%v", rotated, err)
	}
	if loaded, _ := Load(path); loaded != rotated {
		t.Fatalf("load after rotate = %q, want %q", loaded, rotated)
	}
}

func TestRequire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	key, _, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	h := Require(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, tc := range []struct {
		header string
		want   int
	}{
		{"", http.StatusUnauthorized},
		{"Bearer wrong", http.StatusUnauthorized},
		{key, http.StatusUnauthorized},
		{"Bearer " + key, http.StatusOK},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("header %q: status %d, want %d", tc.header, rec.Code, tc.want)
		}
	}
}
