package persist

import (
	"atlas-render/internal/model"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Config carries deployment-tunable storage protection settings.
type Config struct {
	MarginBytes  uint64
	MarginInodes uint64
	FaultFull    bool
}

type Store struct {
	mu    sync.RWMutex
	path  string
	state model.State
	lock  *os.File
	write func(string, []byte) error
	guard *DiskGuard
}

func Open(path string, cfg Config) (*Store, error) {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	lock, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, fmt.Errorf("state is already opened by another process: %w", e)
	}
	if cfg.MarginBytes == 0 {
		cfg.MarginBytes = DefaultMarginBytes
	}
	s := &Store{path: path, state: model.NewState(), lock: lock, write: atomicWrite, guard: NewDiskGuard(dir, cfg.MarginBytes, cfg.MarginInodes, cfg.FaultFull)}
	data, e := os.ReadFile(path)
	if e != nil && !os.IsNotExist(e) {
		s.Close()
		return nil, e
	}
	if e == nil {
		// An empty or whitespace-only file cannot be a valid state snapshot.
		// Refuse to start instead of treating it as a fresh store, which would
		// let the first commit overwrite an accidentally truncated database.
		if len(strings.TrimSpace(string(data))) == 0 {
			s.Close()
			return nil, fmt.Errorf("state file %s is empty; restore from backup before starting", path)
		}
		if e = json.Unmarshal(data, &s.state); e != nil {
			s.Close()
			return nil, fmt.Errorf("invalid persisted JSON: %w", e)
		}
		if e = Validate(s.state); e != nil {
			s.Close()
			return nil, e
		}
	}
	// Remove temp files left by a process that died between write and rename.
	// They are never the source of truth and would otherwise hold disk space.
	if entries, e := os.ReadDir(dir); e == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), tempPrefix) {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	e := s.lock.Close()
	s.lock = nil
	return e
}
func (s *Store) Read() model.State { s.mu.RLock(); defer s.mu.RUnlock(); return model.Clone(s.state) }

// Disk exposes the state-volume guard for status and health reporting.
func (s *Store) Disk() *DiskGuard { return s.guard }

// Update runs fn against a cloned state and commits it durably. Admission is
// checked before any business mutation so the service never gives callers a
// promise (job, receipt, snapshot) that cannot be persisted. A failed write
// keeps the previous in-memory state readable and unchanged.
func (s *Store) Update(fn func(*model.State) error) error {
	if e := s.guard.Check(); e != nil {
		return e
	}
	return s.commit(fn)
}

// UpdateRelief commits a state-shrinking transaction (result purge) even while
// the disk guard is rejecting ordinary writes, because freeing bytes is part
// of recovery. The same durability and validation rules still apply.
func (s *Store) UpdateRelief(fn func(*model.State) error) error {
	return s.commit(fn)
}

func (s *Store) commit(fn func(*model.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := model.Clone(s.state)
	if e := fn(&next); e != nil {
		return e
	}
	if e := Validate(next); e != nil {
		return e
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	if e = s.write(s.path, b); e != nil {
		if d, ok := classifyWriteError(e).(*model.Error); ok {
			s.guard.MarkFull(d.Message)
			return d
		}
		return fmt.Errorf("persist state: %w", e)
	}
	s.state = next
	return nil
}

const tempPrefix = ".atlas-write-"

func atomicWrite(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), tempPrefix)
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	return os.Rename(name, path)
}
