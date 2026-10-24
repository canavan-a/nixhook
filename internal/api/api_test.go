package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/canavan-a/nixhook/internal/auth"
	"github.com/canavan-a/nixhook/internal/job"
)

func newTestServer(t *testing.T) (*httptest.Server, *job.Store, string) {
	t.Helper()
	dir := t.TempDir()
	key, _, err := auth.LoadOrCreate(KeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{StateDir: dir, Source: "/nonexistent", Allow: []string{"box"}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s.store, key
}

func request(t *testing.T, method, url, key, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAuthAndValidation(t *testing.T) {
	ts, _, key := newTestServer(t)

	if code, _ := request(t, "GET", ts.URL+"/healthz", "", ""); code != http.StatusOK {
		t.Errorf("healthz: %d", code)
	}
	if code, _ := request(t, "GET", ts.URL+"/v1/jobs", "", ""); code != http.StatusUnauthorized {
		t.Errorf("jobs without key: %d", code)
	}
	if code, body := request(t, "GET", ts.URL+"/v1/targets", key, ""); code != http.StatusOK || !strings.Contains(body, `"box"`) {
		t.Errorf("targets: %d %s", code, body)
	}
	for _, body := range []string{
		`{}`,
		`{"target":"other"}`,
		`{"target":"bad;name"}`,
		`{"target":"box","ref":"--upload-pack=x"}`,
		`not json`,
	} {
		if code, resp := request(t, "POST", ts.URL+"/v1/deploy", key, body); code != http.StatusBadRequest {
			t.Errorf("deploy %s: %d %s", body, code, resp)
		}
	}
	if code, _ := request(t, "GET", ts.URL+"/v1/jobs/latest", key, ""); code != http.StatusNotFound {
		t.Errorf("latest on empty store: %d", code)
	}
	if code, _ := request(t, "GET", ts.URL+"/v1/jobs/..%2Fkey", key, ""); code != http.StatusNotFound {
		t.Errorf("traversal id: %d", code)
	}
}

func TestLogStream(t *testing.T) {
	ts, store, key := newTestServer(t)

	j := job.New("box", "main", true)
	if err := store.Create(j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.LogPath(j.ID), []byte("one\ntwo\r\nthree"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	j.Status = job.Succeeded
	j.FinishedAt = &now
	if err := store.Save(j); err != nil {
		t.Fatal(err)
	}

	code, body := request(t, "GET", ts.URL+"/v1/jobs/"+j.ID+"/logs?follow=1", key, "")
	if code != http.StatusOK {
		t.Fatalf("logs: %d %s", code, body)
	}
	for _, want := range []string{
		"event: log\nid: 4\ndata: one\n\n",
		"event: log\nid: 9\ndata: two\n\n",
		"event: log\nid: 14\ndata: three\n\n",
		"event: state\n",
		"event: done\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}

	// Resuming from an offset skips what was already delivered.
	_, body = request(t, "GET", ts.URL+"/v1/jobs/"+j.ID+"/logs?offset=9", key, "")
	if strings.Contains(body, "data: one") || !strings.Contains(body, "data: three") {
		t.Errorf("resume from offset 9:\n%s", body)
	}
}

func TestReadLinesHoldsPartialLine(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("done\npartial")

	lines, err := readLines(f, 0, false)
	if err != nil || len(lines) != 1 || lines[0].text != "done" {
		t.Fatalf("not final: %+v %v", lines, err)
	}
	if lines, _ := readLines(f, 5, false); len(lines) != 0 {
		t.Fatalf("partial line returned early: %+v", lines)
	}
	lines, _ = readLines(f, 5, true)
	if len(lines) != 1 || !bytes.Equal([]byte(lines[0].text), []byte("partial")) {
		t.Fatalf("final: %+v", lines)
	}
}
