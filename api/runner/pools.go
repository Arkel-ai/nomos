package runner

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/types"
)

// DIFFERENTIATED POOLS, not one goroutine pool (§4.2).
//
// A single pool is the wrong shape for this workload in two directions at
// once: `pdftoppm | tesseract` and `pandoc` are CPU-bound and would starve the
// HTTP handlers sharing this process (there is no worker Deployment — §2), and
// a cheap db-only state transition would queue behind a three-minute drafting
// call. So each pool has its own claim loop, its own concurrency budget and
// its own slice of the queue:
//
//	ocr    = 1 per pod   CPU-bound, hard page/pixel/time limits
//	render = 1 per pod   pandoc, per-job temp dir
//	llm    = 2 per pod   slow but idle — network wait, not CPU
//	db     = 8 per pod   cheap transitions, and the catch-all
//
// The budgets are per POD, not per cluster: two replicas run two of each.

type pool struct {
	name string
	size int
	// wake is nudged by Enqueue and by a finishing worker, so a freshly
	// queued step does not wait out PollInterval.
	wake chan struct{}
}

var (
	wakeMu    sync.Mutex
	wakeChans []chan struct{}
)

func poolsFrom(cfg config.Config) []*pool {
	specs := []struct {
		name string
		size int
	}{
		{types.StepKindOCR, cfg.StepConcurrencyOCR},
		{types.StepKindRender, cfg.StepConcurrencyRender},
		{types.StepKindLLM, cfg.StepConcurrencyLLM},
		{types.StepKindDB, cfg.StepConcurrencyDB},
	}
	out := make([]*pool, 0, len(specs))
	wakeMu.Lock()
	defer wakeMu.Unlock()
	for _, s := range specs {
		if s.size <= 0 {
			s.size = 1
		}
		p := &pool{name: s.name, size: s.size, wake: make(chan struct{}, 1)}
		wakeChans = append(wakeChans, p.wake)
		out = append(out, p)
	}
	return out
}

// notifyPools nudges every claim loop. Non-blocking: a pool that is already
// awake needs no second nudge.
func notifyPools() {
	wakeMu.Lock()
	defer wakeMu.Unlock()
	for _, ch := range wakeChans {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (p *pool) run(ctx context.Context, db *gorm.DB) {
	log.Debugf("Pool %s started (concurrency=%d)", p.name, p.size)
	sem := make(chan struct{}, p.size)
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			log.Debugf("Pool %s stopped", p.name)
			return
		}

		free := p.size - len(sem)
		progressed := false
		if free > 0 {
			items, err := claimSteps(ctx, db, p.name, free)
			if err != nil {
				if ctx.Err() == nil {
					log.Errorf("Pool %s claim failed: %v", p.name, err)
				}
			} else if len(items) > 0 {
				progressed = true
				log.Debugf("Pool %s claimed %d step(s)", p.name, len(items))
				for _, it := range items {
					sem <- struct{}{}
					go func(it claimed) {
						defer func() {
							<-sem
							select {
							case p.wake <- struct{}{}:
							default:
							}
						}()
						sc := &StepCtx{
							Run:         it.run,
							Step:        it.step,
							Attempt:     it.attempt,
							TenantID:    it.run.TenantID,
							DossierID:   it.run.DossierID,
							ExecutionID: it.attempt.ExecutionID,
							WorkerID:    workerID,
							db:          db,
						}
						execute(ctx, db, sc)
					}(it)
				}
			}
		}

		if progressed {
			// A full batch means there may be more waiting; loop without
			// sleeping. The claim is bounded, so this cannot spin on an
			// empty queue.
			continue
		}
		select {
		case <-ctx.Done():
			log.Debugf("Pool %s stopped", p.name)
			return
		case <-ticker.C:
		case <-p.wake:
		}
	}
}
