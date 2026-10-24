// Package job holds deploy job state on disk and the runner that executes a
// deploy.
package job

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// Status is the state of a job or one of its stages.
type Status string

const (
	Queued    Status = "queued"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	Skipped   Status = "skipped"
)

// Done reports whether the status is final.
func (s Status) Done() bool { return s == Succeeded || s == Failed || s == Skipped }

// Stage names, in the order they run.
const (
	StageSync   = "sync"
	StageBuild  = "build"
	StageSwitch = "switch"
	StageVerify = "verify"
)

// Stage is one step of a deploy.
type Stage struct {
	Name       string     `json:"name"`
	Status     Status     `json:"status"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Duration is how long the stage ran, or zero if it has not finished.
func (s Stage) Duration() time.Duration {
	if s.StartedAt == nil || s.FinishedAt == nil {
		return 0
	}
	return s.FinishedAt.Sub(*s.StartedAt)
}

// Job is a single deploy request and its progress.
type Job struct {
	ID         string     `json:"id"`
	Target     string     `json:"target"`
	Ref        string     `json:"ref"`
	DryRun     bool       `json:"dryRun"`
	Status     Status     `json:"status"`
	Stage      string     `json:"stage,omitempty"`
	Stages     []Stage    `json:"stages"`
	RevBefore  string     `json:"revBefore,omitempty"`
	RevAfter   string     `json:"revAfter,omitempty"`
	GenBefore  int        `json:"genBefore,omitempty"`
	GenAfter   int        `json:"genAfter,omitempty"`
	OutPath    string     `json:"outPath,omitempty"`
	RolledBack bool       `json:"rolledBack,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// New returns a queued job. A dry run stops after the build.
func New(target, ref string, dryRun bool) *Job {
	names := []string{StageSync, StageBuild}
	if !dryRun {
		names = append(names, StageSwitch, StageVerify)
	}
	stages := make([]Stage, len(names))
	for i, name := range names {
		stages[i] = Stage{Name: name, Status: Queued}
	}
	now := time.Now().UTC()
	return &Job{
		ID:        newID(now),
		Target:    target,
		Ref:       ref,
		DryRun:    dryRun,
		Status:    Queued,
		Stages:    stages,
		CreatedAt: now,
	}
}

// Duration is how long the job has been running, or ran in total.
func (j *Job) Duration() time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	if j.FinishedAt == nil {
		return time.Since(*j.StartedAt)
	}
	return j.FinishedAt.Sub(*j.StartedAt)
}

// Fail marks the job and its running stage as failed and skips the stages that
// never started.
func (j *Job) Fail(msg string) {
	now := time.Now().UTC()
	for i := range j.Stages {
		switch st := &j.Stages[i]; st.Status {
		case Running:
			st.Status = Failed
			st.FinishedAt = &now
			if st.Error == "" {
				st.Error = msg
			}
		case Queued:
			st.Status = Skipped
		}
	}
	j.Status = Failed
	j.Error = msg
	j.FinishedAt = &now
}

var idRe = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{6}$`)

// ValidID reports whether id has the shape New produces, which also keeps it
// safe to use as a directory name.
func ValidID(id string) bool { return idRe.MatchString(id) }

func newID(t time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return t.Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// ErrNotFound is returned for an unknown job ID.
var ErrNotFound = errors.New("job not found")

// Store keeps jobs as <dir>/<id>/job.json with the output in <dir>/<id>/log.
type Store struct {
	dir string
}

// NewStore returns a store rooted in stateDir.
func NewStore(stateDir string) *Store {
	return &Store{dir: filepath.Join(stateDir, "jobs")}
}

// Create writes a new job and its empty log.
func (s *Store) Create(j *Job) error {
	if err := os.MkdirAll(filepath.Join(s.dir, j.ID), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.LogPath(j.ID), nil, 0o600); err != nil {
		return err
	}
	return s.Save(j)
}

// Save atomically replaces the stored job.
func (s *Store) Save(j *Job) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, j.ID, "job.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get loads a job by ID.
func (s *Store) Get(id string) (*Job, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir, id, "job.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var j Job
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("job %s: %w", id, err)
	}
	return &j, nil
}

// List returns up to limit jobs, newest first. A limit of 0 returns all.
func (s *Store) List(limit int) ([]*Job, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	jobs := make([]*Job, 0, len(ids))
	for _, id := range ids {
		j, err := s.Get(id)
		if err != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// Latest returns the newest job.
func (s *Store) Latest() (*Job, error) {
	jobs, err := s.List(1)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, ErrNotFound
	}
	return jobs[0], nil
}

// Prune deletes all but the newest keep jobs.
func (s *Store) Prune(keep int) error {
	ids, err := s.ids()
	if err != nil || len(ids) <= keep {
		return err
	}
	for _, id := range ids[keep:] {
		if err := os.RemoveAll(filepath.Join(s.dir, id)); err != nil {
			return err
		}
	}
	return nil
}

// LogPath is the file a job's command output is appended to.
func (s *Store) LogPath(id string) string {
	return filepath.Join(s.dir, id, "log")
}

// ids returns the stored job IDs, newest first.
func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	slices.Sort(ids)
	slices.Reverse(ids)
	return ids, nil
}
