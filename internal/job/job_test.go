package job

import (
	"os"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	j := New("box", "main", false)
	if !ValidID(j.ID) {
		t.Fatalf("invalid id %q", j.ID)
	}
	if len(j.Stages) != 4 {
		t.Fatalf("got %d stages, want 4", len(j.Stages))
	}
	if dry := New("box", "main", true); len(dry.Stages) != 2 || dry.Stages[1].Name != StageBuild {
		t.Fatalf("dry run stages = %+v", dry.Stages)
	}
	for _, id := range []string{"", "../etc", "20261023T153012Z-ABCDEF", "20261023T153012Z-ab12cd/.."} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
}

func TestStore(t *testing.T) {
	s := NewStore(t.TempDir())

	if _, err := s.Latest(); err != ErrNotFound {
		t.Fatalf("Latest on empty store: %v", err)
	}

	var ids []string
	for i := range 3 {
		j := New("box", "main", false)
		// Make IDs strictly increasing regardless of clock resolution.
		j.ID = time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format("20060102T150405Z") + "-abcdef"
		if err := s.Create(j); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}

	got, err := s.Get(ids[0])
	if err != nil || got.Target != "box" || got.Status != Queued {
		t.Fatalf("Get: %+v %v", got, err)
	}
	if _, err := os.Stat(s.LogPath(ids[0])); err != nil {
		t.Fatalf("log not created: %v", err)
	}

	got.Stages[0].Status = Running
	got.Fail("boom")
	if err := s.Save(got); err != nil {
		t.Fatal(err)
	}
	reread, _ := s.Get(ids[0])
	if reread.Status != Failed || reread.Stages[0].Status != Failed || reread.Stages[0].Error != "boom" {
		t.Fatalf("after Fail: %+v", reread)
	}

	latest, err := s.Latest()
	if err != nil || latest.ID != ids[2] {
		t.Fatalf("Latest = %v %v, want %s", latest, err, ids[2])
	}

	if err := s.Prune(2); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.List(0)
	if len(jobs) != 2 || jobs[0].ID != ids[2] || jobs[1].ID != ids[1] {
		t.Fatalf("after prune: %d jobs", len(jobs))
	}
	if _, err := s.Get(ids[0]); err != ErrNotFound {
		t.Fatalf("pruned job still readable: %v", err)
	}
}
