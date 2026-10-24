// Package api is the nixhook HTTP server and the request and response types
// shared with the client.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/canavan-a/nixhook/internal/auth"
	"github.com/canavan-a/nixhook/internal/flake"
	"github.com/canavan-a/nixhook/internal/job"
	"github.com/canavan-a/nixhook/internal/system"
)

const (
	// DefaultRef is deployed when a request does not name one.
	DefaultRef = "main"

	keepJobs     = 50
	pollInterval = 500 * time.Millisecond
	// pingInterval keeps idle log streams alive through proxies such as
	// Cloudflare, which drop connections that are silent for 100s.
	pingInterval = 15 * time.Second
	// reconcileEvery is how many polls a log stream waits between checks
	// that its job's runner is still alive.
	reconcileEvery = 10
)

// DeployRequest is the body of POST /v1/deploy. Every field is optional when
// the server has a default target.
type DeployRequest struct {
	Target string `json:"target,omitempty"`
	Ref    string `json:"ref,omitempty"`
	DryRun bool   `json:"dryRun,omitempty"`
}

// DeployResponse is returned when a deploy has been started.
type DeployResponse struct {
	JobID string   `json:"jobId"`
	Job   *job.Job `json:"job"`
}

// Status describes the box.
type Status struct {
	Hostname     string   `json:"hostname"`
	NixOSVersion string   `json:"nixosVersion,omitempty"`
	Generation   int      `json:"generation,omitempty"`
	Flake        string   `json:"flake"`
	Rev          string   `json:"rev,omitempty"`
	State        string   `json:"state,omitempty"`
	FailedUnits  []string `json:"failedUnits"`
	ActiveJob    *job.Job `json:"activeJob,omitempty"`
}

// TargetsResponse lists the flake tags the box will deploy.
type TargetsResponse struct {
	Targets []string `json:"targets"`
}

// ErrorResponse is the body of every non-2xx JSON response.
type ErrorResponse struct {
	Error   string   `json:"error"`
	Targets []string `json:"targets,omitempty"`
	Job     *job.Job `json:"job,omitempty"`
}

// Config configures a Server.
type Config struct {
	StateDir string
	Source   flake.Source
	// Allow restricts which targets may be deployed. Empty allows any
	// target the flake defines.
	Allow         []string
	DefaultTarget string
	Rollback      bool
	// Exe is the nixhook binary the deploy unit runs.
	Exe string
	// Env is passed into the deploy unit as KEY=VALUE pairs.
	Env []string
}

// KeyPath is where the API key lives inside stateDir.
func KeyPath(stateDir string) string { return filepath.Join(stateDir, "key") }

// Server serves the nixhook API.
type Server struct {
	cfg   Config
	store *job.Store
	// mu serializes starting jobs with reconciling them, so a job that was
	// just created is never mistaken for one whose runner died.
	mu sync.Mutex
}

// New returns a server for cfg.
func New(cfg Config) *Server {
	return &Server{cfg: cfg, store: job.NewStore(cfg.StateDir)}
}

// Handler returns the HTTP handler with authentication applied to /v1.
func (s *Server) Handler() http.Handler {
	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/status", s.status)
	v1.HandleFunc("GET /v1/targets", s.targets)
	v1.HandleFunc("POST /v1/deploy", s.deploy)
	v1.HandleFunc("GET /v1/jobs", s.jobs)
	v1.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	v1.HandleFunc("GET /v1/jobs/{id}/logs", s.logs)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})
	mux.Handle("/v1/", auth.Require(KeyPath(s.cfg.StateDir), v1))
	return logRequests(mux)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := Status{
		Flake:        string(s.cfg.Source),
		NixOSVersion: system.NixOSVersion(),
		State:        system.State(ctx),
		ActiveJob:    s.reconcile(ctx),
	}
	st.Hostname, _ = os.Hostname()
	st.Generation, _ = system.Generation()
	if s.cfg.Source.Local() {
		st.Rev, _ = s.cfg.Source.Rev(ctx, DefaultRef)
	}
	failed, err := system.FailedUnits(ctx)
	if err != nil {
		slog.Warn("list failed units", "err", err)
	}
	st.FailedUnits = failed
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) targets(w http.ResponseWriter, r *http.Request) {
	if len(s.cfg.Allow) > 0 {
		writeJSON(w, http.StatusOK, TargetsResponse{Targets: s.cfg.Allow})
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = DefaultRef
	}
	if !flake.ValidRef(ref) {
		writeError(w, http.StatusBadRequest, ErrorResponse{Error: "invalid ref"})
		return
	}
	targets, err := s.cfg.Source.Targets(r.Context(), s.cfg.Source.URI(ref))
	if err != nil {
		writeError(w, http.StatusBadGateway, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, TargetsResponse{Targets: targets})
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	var req DeployRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, ErrorResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.Target == "" {
		req.Target = s.cfg.DefaultTarget
	}
	if req.Ref == "" {
		req.Ref = DefaultRef
	}
	switch {
	case req.Target == "":
		writeError(w, http.StatusBadRequest, ErrorResponse{Error: "target is required", Targets: s.cfg.Allow})
		return
	case !flake.ValidTarget(req.Target):
		writeError(w, http.StatusBadRequest, ErrorResponse{Error: "invalid target"})
		return
	case len(s.cfg.Allow) > 0 && !slices.Contains(s.cfg.Allow, req.Target):
		writeError(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("target %q is not allowed on this box", req.Target), Targets: s.cfg.Allow})
		return
	case !flake.ValidRef(req.Ref):
		writeError(w, http.StatusBadRequest, ErrorResponse{Error: "invalid ref"})
		return
	}

	// Starting the unit must not be abandoned halfway because the client
	// hung up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()

	s.mu.Lock()
	defer s.mu.Unlock()
	if active := s.reconcileLocked(ctx); active != nil || system.JobActive(ctx) {
		writeError(w, http.StatusConflict, ErrorResponse{Error: system.ErrJobRunning.Error(), Job: active})
		return
	}

	j := job.New(req.Target, req.Ref, req.DryRun)
	if err := s.store.Create(j); err != nil {
		writeError(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	if err := s.store.Prune(keepJobs); err != nil {
		slog.Warn("prune jobs", "err", err)
	}

	args := []string{"run-job", "--state-dir", s.cfg.StateDir, "--flake", string(s.cfg.Source)}
	if s.cfg.Rollback {
		args = append(args, "--rollback")
	}
	args = append(args, j.ID)
	desc := fmt.Sprintf("nixhook deploy %s (%s)", j.Target, j.ID)
	if err := system.StartJob(ctx, desc, s.cfg.Env, s.cfg.Exe, args...); err != nil {
		j.Fail("start deploy unit: " + err.Error())
		if saveErr := s.store.Save(j); saveErr != nil {
			slog.Error("save job", "job", j.ID, "err", saveErr)
		}
		status := http.StatusInternalServerError
		if errors.Is(err, system.ErrJobRunning) {
			status = http.StatusConflict
		}
		writeError(w, status, ErrorResponse{Error: err.Error(), Job: j})
		return
	}
	slog.Info("deploy started", "job", j.ID, "target", j.Target, "ref", j.Ref, "dryRun", j.DryRun)
	writeJSON(w, http.StatusAccepted, DeployResponse{JobID: j.ID, Job: j})
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	s.reconcile(r.Context())
	jobs, err := s.store.List(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	s.reconcile(r.Context())
	j, ok := s.lookup(w, r.PathValue("id"))
	if ok {
		writeJSON(w, http.StatusOK, j)
	}
}

// logs streams a job's log as server-sent events: "log" events carry one line
// each with the byte offset after it as the event ID, "state" events carry the
// job whenever it changes, and a final "done" event carries the finished job.
// Without follow=1 the stream ends once the current log has been sent.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	j, ok := s.lookup(w, r.PathValue("id"))
	if !ok {
		return
	}
	id := j.ID
	offset := parseOffset(r)
	q := r.URL.Query().Get("follow")
	follow := q == "1" || q == "true"

	f, err := os.Open(s.store.LogPath(id))
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	ctx := r.Context()
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	var lastState []byte
	for polls := 1; ; polls++ {
		// Read the state before the log: the runner writes its last log
		// line before its final state, so once the state says done the
		// log is complete.
		j, err := s.store.Get(id)
		if err != nil {
			writeEvent(w, "error", "", []byte(strconv.Quote(err.Error())))
			rc.Flush()
			return
		}
		for {
			lines, err := readLines(f, offset, j.Status.Done())
			if err != nil {
				writeEvent(w, "error", "", []byte(strconv.Quote(err.Error())))
				rc.Flush()
				return
			}
			if len(lines) == 0 {
				break
			}
			for _, l := range lines {
				offset += l.size
				writeEvent(w, "log", strconv.FormatInt(offset, 10), []byte(l.text))
			}
		}
		state, _ := json.Marshal(j)
		if !bytes.Equal(state, lastState) {
			writeEvent(w, "state", "", state)
			lastState = state
		}
		if j.Status.Done() {
			writeEvent(w, "done", "", state)
			rc.Flush()
			return
		}
		if err := rc.Flush(); err != nil || !follow {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
		case <-poll.C:
			if polls%reconcileEvery == 0 {
				s.reconcile(ctx)
			}
		}
	}
}

// lookup resolves an ID, or "latest", writing a 404 if there is no such job.
func (s *Server) lookup(w http.ResponseWriter, id string) (*job.Job, bool) {
	var j *job.Job
	var err error
	if id == "latest" {
		j, err = s.store.Latest()
	} else {
		j, err = s.store.Get(id)
	}
	switch {
	case errors.Is(err, job.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorResponse{Error: "job not found"})
		return nil, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return nil, false
	}
	return j, true
}

func (s *Server) reconcile(ctx context.Context) *job.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileLocked(ctx)
}

// reconcileLocked returns the job that is currently running, if any. Jobs
// that claim to be running while the deploy unit is gone (the runner crashed,
// or the box rebooted) are marked failed.
func (s *Server) reconcileLocked(ctx context.Context) *job.Job {
	jobs, err := s.store.List(keepJobs)
	if err != nil {
		slog.Warn("list jobs", "err", err)
		return nil
	}
	var pending []*job.Job
	for _, j := range jobs {
		if !j.Status.Done() {
			pending = append(pending, j)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if system.JobActive(ctx) {
		return pending[0]
	}
	for _, j := range pending {
		// The unit is gone, so the runner has written everything it will
		// ever write. Re-read in case it finished after the listing.
		j, err := s.store.Get(j.ID)
		if err != nil || j.Status.Done() {
			continue
		}
		j.Fail("job runner exited without finishing")
		if err := s.store.Save(j); err != nil {
			slog.Error("save job", "job", j.ID, "err", err)
		}
	}
	return nil
}

func parseOffset(r *http.Request) int64 {
	v := r.URL.Query().Get("offset")
	if v == "" {
		v = r.Header.Get("Last-Event-ID")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

type line struct {
	text string
	// size is the number of bytes the line occupied in the file, including
	// its newline.
	size int64
}

const readChunk = 256 << 10

// readLines returns the complete lines in f after offset. A trailing partial
// line is returned only when final is set or it fills a whole chunk, so a line
// is never split while it is still being written.
func readLines(f *os.File, offset int64, final bool) ([]line, error) {
	buf := make([]byte, readChunk)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]
	if end := bytes.LastIndexByte(buf, '\n'); end >= 0 {
		buf = buf[:end+1]
	} else if !final && n < readChunk {
		return nil, nil
	}
	var lines []line
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		raw := buf
		if i >= 0 {
			raw = buf[:i+1]
		}
		buf = buf[len(raw):]
		text := strings.TrimRight(string(raw), "\r\n")
		lines = append(lines, line{text: strings.ReplaceAll(text, "\r", ""), size: int64(len(raw))})
	}
	return lines, nil
}

func writeEvent(w io.Writer, event, id string, data []byte) {
	fmt.Fprintf(w, "event: %s\n", event)
	if id != "" {
		fmt.Fprintf(w, "id: %s\n", id)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e ErrorResponse) {
	writeJSON(w, status, e)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer to flush
// log streams.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		remote := r.Header.Get("CF-Connecting-IP")
		if remote == "" {
			remote = r.RemoteAddr
		}
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"remote", remote, "duration", time.Since(start).Round(time.Millisecond))
	})
}
