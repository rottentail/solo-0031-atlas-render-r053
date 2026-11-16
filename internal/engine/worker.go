package engine

import (
	"atlas-render/internal/codec"
	"atlas-render/internal/model"
	"atlas-render/internal/persist"
	"atlas-render/internal/raster"
	"context"
	"errors"
	"log"
	"sort"
	"sync"
	"time"
)

type Worker struct {
	store  *persist.Store
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func New(st *persist.Store) *Worker {
	return &Worker{store: st, active: map[string]context.CancelFunc{}}
}
func (w *Worker) Recover() error {
	// At startup no execution contexts exist, so every running job is an orphan.
	// If the volume is currently unwritable the recovery commit must wait:
	// return the disk_full error so main can enter degraded read-only mode,
	// and the runtime sweep will requeue once writes work again.
	if e := w.store.Disk().Check(); e != nil {
		return e
	}
	return w.requeueRunning(nil, "job.recovered")
}

// requeueRunning returns running jobs to the queue without incrementing
// attempts. When onlyIDs is non-nil, just those IDs are touched; nil requeues
// every running job (startup recovery). Used when a result commit could not be
// durably recorded (for example the state volume filled mid-render): the job
// stays "running" in the surviving state until writes work again, then is
// replayed with its fixed input.
func (w *Worker) requeueRunning(onlyIDs map[string]bool, kind string) error {
	return w.store.UpdateRelief(func(s *model.State) error {
		for id, j := range s.Jobs {
			if j.State != "running" || (onlyIDs != nil && !onlyIDs[id]) {
				continue
			}
			j.State = "queued"
			j.UpdatedAt = time.Now().UTC()
			s.Jobs[id] = j
			s.Emit(kind, id)
		}
		return nil
	})
}
func (w *Worker) Cancel(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cancel := w.active[id]; cancel != nil {
		cancel()
	}
}
func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if e := w.step(ctx); e != nil {
				log.Printf("render worker: %v", e)
			}
		}
	}
}
func (w *Worker) claim() (model.Job, bool, error) {
	var job model.Job
	found := false
	// Do not start new work while durable commits are impossible; an accepted
	// claim that cannot later commit would strand the job in "running".
	if e := w.store.Disk().Check(); e != nil {
		return job, false, nil
	}
	state := w.store.Read()
	ids := []string{}
	for id, j := range state.Jobs {
		if j.State == "queued" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return job, false, nil
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := state.Jobs[ids[i]], state.Jobs[ids[j]]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	e := w.store.Update(func(s *model.State) error {
		j := s.Jobs[ids[0]]
		if j.State != "queued" {
			return nil
		}
		j.State = "running"
		j.UpdatedAt = time.Now().UTC()
		s.Jobs[j.ID] = j
		s.Emit("job.running", j.ID)
		job = j
		found = true
		return nil
	})
	if isDiskFull(e) {
		// Guard flipped between the check and the commit; wait for recovery.
		return job, false, nil
	}
	return job, found, e
}

func (w *Worker) step(parent context.Context) error {
	// Requeue jobs whose running attempt could not commit before space ran
	// out. Reads show whether any such orphans exist; the update itself is a
	// relief write so it can land the moment the volume is writable again.
	if ids := w.orphanRunning(); len(ids) > 0 {
		// While the volume stays full the relief write cannot land; that is
		// an expected condition, so keep retrying quietly instead of logging.
		if e := w.requeueRunning(ids, "job.requeued"); e != nil && !isDiskFull(e) {
			return e
		}
		return nil
	}
	j, ok, e := w.claim()
	if e != nil || !ok {
		return e
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w.mu.Lock()
	w.active[j.ID] = cancel
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.active, j.ID); w.mu.Unlock() }()
	// Cancellation can commit between claim and tracking; consult the committed state.
	current := w.store.Read().Jobs[j.ID]
	if current.State != "running" {
		cancel()
	}
	plan, e := raster.Compile(j.Payload, j.Scale)
	var data []byte
	if e == nil {
		var imgErr error
		img, imgErr := plan.Render(ctx)
		e = imgErr
		if e == nil {
			data, e = codec.Encode(img)
		}
	}
	if parent.Err() != nil {
		return nil
	}
	// A full volume at result-commit time is an expected retry condition: the
	// job stays "running" in state and the orphan sweep requeues it once space
	// returns. Swallow the domain error so Run does not log every 50ms tick.
	e = w.store.Update(func(s *model.State) error {
		v := s.Jobs[j.ID]
		if v.State != "running" || v.Attempt != j.Attempt {
			return nil
		}
		v.UpdatedAt = time.Now().UTC()
		if e != nil {
			if errors.Is(e, context.Canceled) {
				v.State = "cancelled"
			} else {
				v.State = "failed"
				v.Error = e.Error()
			}
			s.Jobs[v.ID] = v
			s.Emit("job."+v.State, v.ID)
			return nil
		}
		a := model.Artifact{ID: model.ID("png", s.Sequence+1), JobID: v.ID, State: "live", Digest: model.Digest(data), Width: plan.Width, Height: plan.Height, PNG: data}
		s.Artifacts[a.ID] = a
		v.State = "succeeded"
		v.ArtifactID = a.ID
		s.Jobs[v.ID] = v
		s.Emit("job.succeeded", v.ID)
		return nil
	})
	if isDiskFull(e) {
		log.Printf("render worker: result for %s deferred, state volume full; will requeue on recovery", j.ID)
		return nil
	}
	return e
}

// orphanRunning returns the set of persisted "running" job IDs that no live
// execution context owns in this process. Such jobs are requeued once writes
// recover.
func (w *Worker) orphanRunning() map[string]bool {
	state := w.store.Read()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]bool{}
	for id, j := range state.Jobs {
		if j.State == "running" && w.active[id] == nil {
			out[id] = true
		}
	}
	return out
}

func isDiskFull(e error) bool {
	var de *model.Error
	return errors.As(e, &de) && de.Code == "disk_full"
}
