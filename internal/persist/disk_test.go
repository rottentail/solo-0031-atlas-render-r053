package persist

import (
	"atlas-render/internal/model"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestClassifyWriteError(t *testing.T) {
	var de *model.Error
	if e := classifyWriteError(nil); e != nil {
		t.Fatalf("nil error should stay nil, got %v", e)
	}
	e := classifyWriteError(syscall.ENOSPC)
	if !errors.As(e, &de) || de.Code != "disk_full" || de.Status != 503 {
		t.Fatalf("ENOSPC must map to disk_full 503, got %v", e)
	}
	e = classifyWriteError(syscall.EDQUOT)
	if !errors.As(e, &de) || de.Code != "disk_full" {
		t.Fatalf("EDQUOT must map to disk_full, got %v", e)
	}
	other := errors.New("boom")
	if classifyWriteError(other) != other {
		t.Fatal("unrelated errors must pass through unchanged")
	}
}

func TestGuardRejectsAndRecovers(t *testing.T) {
	dir := t.TempDir()
	g := NewDiskGuard(dir, 1<<62, 0, false)
	// Reservation larger than any real volume forces a non-writable verdict.
	if e := g.Check(); e == nil {
		t.Fatal("expected disk_full from oversized byte reservation")
	} else {
		var de *model.Error
		if !errors.As(e, &de) || de.Code != "disk_full" {
			t.Fatalf("expected disk_full, got %v", e)
		}
	}
	st := g.Snapshot()
	if st.Writable || st.FreeBytes == 0 || st.TotalBytes == 0 {
		t.Fatalf("unexpected probe snapshot: %+v", st)
	}
	// A zero reservation on a healthy volume must pass.
	ok := NewDiskGuard(dir, 0, 0, false)
	if e := ok.Check(); e != nil {
		t.Fatalf("zero reservation should be writable: %v", e)
	}
	// MarkFull flips the cached verdict immediately even with margin zero;
	// it stays visible until the short probe cache expires and rechecks.
	ok.MarkFull("injected write failure")
	if got := ok.Snapshot(); got.Writable || got.Reason != "injected write failure" {
		t.Fatalf("MarkFull must pin a non-writable cached verdict: %+v", got)
	}
}

func TestFaultInjection(t *testing.T) {
	g := NewDiskGuard(t.TempDir(), 0, 0, true)
	st := g.Snapshot()
	if st.Writable || !st.FaultInjected || st.Reason == "" {
		t.Fatalf("fault injection must report non-writable: %+v", st)
	}
}

func TestOpenRejectsEmptyStateAndSweepsTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if e := os.WriteFile(path, []byte("  \n "), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(path, Config{}); e == nil {
		t.Fatal("empty state file must be rejected, not treated as a fresh store")
	}
	// Corrupt JSON is rejected and left untouched.
	bad := []byte(`{"version":1`)
	if e := os.WriteFile(path, bad, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(path, Config{}); e == nil {
		t.Fatal("corrupt state must be rejected")
	}
	if got, _ := os.ReadFile(path); string(got) != string(bad) {
		t.Fatal("rejected state file must not be overwritten")
	}
	// A fresh open succeeds and sweeps stale temp files from a prior crash.
	if e := os.Remove(path); e != nil {
		t.Fatal(e)
	}
	stale := filepath.Join(dir, tempPrefix+"stale")
	if e := os.WriteFile(stale, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	st, e := Open(path, Config{MarginBytes: 0, MarginInodes: 0})
	if e != nil {
		t.Fatalf("fresh open failed: %v", e)
	}
	defer st.Close()
	if _, e := os.Stat(stale); !os.IsNotExist(e) {
		t.Fatal("stale temp file should be removed on open")
	}
}

func TestUpdateFailsClosedAndKeepsState(t *testing.T) {
	dir := t.TempDir()
	st, e := Open(filepath.Join(dir, "state.json"), Config{MarginBytes: 0, MarginInodes: 0})
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	// Force every commit to look full; the in-memory state must stay unchanged
	// and a domain disk_full error must surface. MarkFull pins the cached
	// verdict so the guard check inside Update rejects before any mutation.
	st.Disk().MarkFull("test full")
	err := st.Update(func(s *model.State) error {
		t.Fatal("transaction body must not run when the guard rejects admission")
		return nil
	})
	var de *model.Error
	if !errors.As(err, &de) || de.Code != "disk_full" {
		t.Fatalf("expected disk_full, got %v", err)
	}
	if got := st.Read().Sequence; got != 0 {
		t.Fatalf("failed commit must not mutate in-memory state, sequence=%d", got)
	}
	// Relief writes (purge path) are still allowed.
	if e := st.UpdateRelief(func(s *model.State) error {
		s.Sequence = 7
		return nil
	}); e != nil {
		t.Fatalf("relief write should succeed while guard is full: %v", e)
	}
	if got := st.Read().Sequence; got != 7 {
		t.Fatalf("relief commit did not land, sequence=%d", got)
	}
}
