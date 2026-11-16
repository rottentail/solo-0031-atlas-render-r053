package persist

import (
	"atlas-render/internal/model"
	"sync"
	"time"
)

// Default space reservations on the state volume. A commit is refused while
// fewer than this many bytes or inodes are available, so the service never
// accepts work it cannot durably record.
const (
	DefaultMarginBytes  = 64 << 20 // 64 MiB
	DefaultMarginInodes = 128
	statusCacheTTL      = 250 * time.Millisecond
)

// DiskStatus is the observable state of the state volume, surfaced through
// GET /v1/status and used for health and admission decisions.
type DiskStatus struct {
	Writable      bool   `json:"writable"`
	Reason        string `json:"reason,omitempty"`
	FaultInjected bool   `json:"faultInjected,omitempty"`
	Path          string `json:"path"`
	FreeBytes     uint64 `json:"freeBytes"`
	TotalBytes    uint64 `json:"totalBytes"`
	FreeInodes    uint64 `json:"freeInodes"`
	MarginBytes   uint64 `json:"marginBytes"`
	MarginInodes  uint64 `json:"marginInodes"`
	CheckedAt     string `json:"checkedAt"`
}

// DiskGuard inspects the filesystem that holds the state file and remembers
// short-lived failures so every admission path shares one verdict.
type DiskGuard struct {
	path         string
	marginBytes  uint64
	marginInodes uint64
	faultFull    bool

	mu        sync.Mutex
	cached    DiskStatus
	checkedAt time.Time
}

func NewDiskGuard(path string, marginBytes, marginInodes uint64, faultFull bool) *DiskGuard {
	return &DiskGuard{path: path, marginBytes: marginBytes, marginInodes: marginInodes, faultFull: faultFull}
}

// Snapshot returns the current volume status, throttling statfs calls.
func (g *DiskGuard) Snapshot() DiskStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.checkedAt) < statusCacheTTL {
		return g.cached
	}
	st := g.probe()
	g.cached = st
	g.checkedAt = time.Now()
	return st
}

// Check returns a disk_full domain error while the volume is too full for a
// durable commit. Read-only operations never call this.
func (g *DiskGuard) Check() error {
	st := g.Snapshot()
	if st.Writable {
		return nil
	}
	return model.DiskFull(st.Reason)
}

// MarkFull records an observed write failure (for example ENOSPC racing the
// cached probe) so subsequent admissions fail closed until the next probe.
func (g *DiskGuard) MarkFull(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cached = g.probeLocked(reason)
	g.checkedAt = time.Now()
}

func (g *DiskGuard) probe() DiskStatus {
	return g.probeLocked("")
}

func (g *DiskGuard) probeLocked(forcedReason string) DiskStatus {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	st := DiskStatus{Path: g.path, MarginBytes: g.marginBytes, MarginInodes: g.marginInodes, CheckedAt: now, FaultInjected: g.faultFull}
	if g.faultFull {
		st.Reason = "disk full fault injection enabled"
		return st
	}
	free, total, freeInodes, e := fsSpace(g.path)
	if e != nil {
		st.Reason = "state volume unavailable: " + e.Error()
		return st
	}
	st.FreeBytes, st.TotalBytes, st.FreeInodes = free, total, freeInodes
	if forcedReason != "" {
		st.Reason = forcedReason
		return st
	}
	if free < g.marginBytes {
		st.Reason = "state volume free bytes below reservation"
		return st
	}
	// A zero inode reservation disables the inode check, for filesystems that
	// do not report meaningful Ffree values (some overlay/network mounts).
	if g.marginInodes > 0 && freeInodes < g.marginInodes {
		st.Reason = "state volume free inodes below reservation"
		return st
	}
	st.Writable = true
	return st
}

// classifyWriteError maps raw filesystem write failures to the disk_full
// domain error; anything else is returned untouched for a generic 500.
func classifyWriteError(e error) error {
	if e == nil {
		return nil
	}
	if isNoSpace(e) {
		return model.DiskFull("state volume has no space for durable commit")
	}
	return e
}
