package engine

import (
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) *persist.Store {
	t.Helper()
	st, e := persist.Open(filepath.Join(t.TempDir(), "state.json"), persist.Config{MarginBytes: 0, MarginInodes: 0})
	if e != nil {
		t.Fatalf("open: %v", e)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedRunningJob(t *testing.T, st *persist.Store) string {
	t.Helper()
	jobID := ""
	e := st.UpdateRelief(func(s *model.State) error {
		// Minimal valid structures satisfying Validate.
		doc := model.Document{Width: 4, Height: 4, Background: "#000000", Layers: []model.Layer{}}
		s.Scenes["sc-1"] = model.Scene{ID: "sc-1", Name: "n", Revision: 1, State: "active", Document: doc, UpdatedAt: time.Now().UTC()}
		snap := model.Snapshot{ID: "snap-00000001", SceneID: "sc-1", Revision: 1, State: "active", Document: doc, Assets: map[string]model.Asset{}}
		snap.Digest = model.SnapshotDigest(snap)
		s.Snapshots[snap.ID] = snap
		j := model.Job{ID: "job-00000001", SnapshotID: snap.ID, State: "running", Scale: 1, Attempt: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Payload: snap}
		s.Jobs[j.ID] = j
		jobID = j.ID
		return nil
	})
	if e != nil {
		t.Fatalf("seed: %v", e)
	}
	return jobID
}

func TestRecoverRequeuesRunningWithoutAttemptBump(t *testing.T) {
	st := openStore(t)
	id := seedRunningJob(t, st)
	w := New(st)
	if e := w.Recover(); e != nil {
		t.Fatalf("recover: %v", e)
	}
	j := st.Read().Jobs[id]
	if j.State != "queued" || j.Attempt != 1 {
		t.Fatalf("expected queued attempt 1, got %s attempt %d", j.State, j.Attempt)
	}
}

func TestRecoverDefersWhenDiskFull(t *testing.T) {
	st := openStore(t)
	id := seedRunningJob(t, st)
	st.Disk().MarkFull("full")
	w := New(st)
	if e := w.Recover(); e == nil {
		t.Fatal("recover must surface disk_full instead of forcing a commit")
	}
	if got := st.Read().Jobs[id].State; got != "running" {
		t.Fatalf("job must stay running while recovery is deferred, got %s", got)
	}
}

func TestOrphanSweepSkipsActiveContexts(t *testing.T) {
	st := openStore(t)
	id := seedRunningJob(t, st)
	w := New(st)
	// Simulate a live execution owning this job: it must not be requeued.
	w.active[id] = func() {}
	if ids := w.orphanRunning(); len(ids) != 0 {
		t.Fatalf("owned running job is not an orphan, got %v", ids)
	}
	delete(w.active, id)
	ids := w.orphanRunning()
	if len(ids) != 1 || !ids[id] {
		t.Fatalf("expected one orphan %s, got %v", id, ids)
	}
	// Probe fresh so the guard cache from the previous test verdict is gone.
	if e := st.Disk().Check(); e != nil {
		t.Fatalf("volume should be writable for the sweep: %v", e)
	}
	if e := w.requeueRunning(ids, "job.requeued"); e != nil {
		t.Fatalf("requeue: %v", e)
	}
	if got := st.Read().Jobs[id].State; got != "queued" {
		t.Fatalf("expected queued after sweep, got %s", got)
	}
}
