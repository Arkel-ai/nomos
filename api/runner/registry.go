package runner

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Arkel-ai/nomos/api/types"
)

// The step registry. A step KIND is the logical name of a unit of work
// ("qualify_piece", "build_classement", "ask_user"), not the pool it runs on:
// the kind DECLARES its pool, which is how a cheap transition and a
// three-minute LLM call end up on different goroutine budgets (§4.2).
//
// types.PoolForStepKind stays the fallback for a kind nobody registered — it
// routes to the db pool, where the claimer fails it with `unknown_step_kind`
// rather than letting it sit queued forever.

// RegisterOptions describes how a step kind is scheduled. Zero values mean
// "use the runner default".
type RegisterOptions struct {
	// Pool is one of types.StepKindDB / StepKindLLM / StepKindOCR /
	// StepKindRender. Empty falls back to types.PoolForStepKind(kind).
	Pool string
	// Lease is how long a claim of this kind holds the row before the reaper
	// may take it back. Empty uses DefaultLease (90 s).
	Lease time.Duration
	// MaxAttempts is the default cap for steps of this kind when the row
	// does not set its own.
	MaxAttempts int
}

type registration struct {
	fn   StepFunc
	opts RegisterOptions
}

var (
	regMu    sync.RWMutex
	registry = map[string]registration{}
)

// Register makes a step kind runnable. Call it from the workflow package's
// init(), before or after Start — the claimer reads the registry on every
// iteration, so a late registration is picked up on the next poll.
//
// Registering the same kind twice panics: two definitions of one kind would
// make a run non-reproducible, which is the whole thing this runner exists to
// prevent.
func Register(kind string, fn StepFunc, opts ...RegisterOptions) {
	if kind == "" || fn == nil {
		panic("runner.Register: kind and fn are required")
	}
	var o RegisterOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Pool == "" {
		o.Pool = types.PoolForStepKind(kind)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic(fmt.Sprintf("runner.Register: step kind %q registered twice", kind))
	}
	registry[kind] = registration{fn: fn, opts: o}
}

func lookup(kind string) (StepFunc, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	r, ok := registry[kind]
	if !ok {
		return nil, false
	}
	return r.fn, true
}

// leaseFor is the per-kind lease used by the claim and by every heartbeat.
func leaseFor(kind string) time.Duration {
	regMu.RLock()
	defer regMu.RUnlock()
	if r, ok := registry[kind]; ok && r.opts.Lease > 0 {
		return r.opts.Lease
	}
	return DefaultLease
}

// maxAttemptsFor is the per-kind default attempt cap.
func maxAttemptsFor(kind string) int {
	regMu.RLock()
	defer regMu.RUnlock()
	if r, ok := registry[kind]; ok && r.opts.MaxAttempts > 0 {
		return r.opts.MaxAttempts
	}
	return DefaultMaxAttempts
}

// kindsForPool lists the registered kinds that run on pool. Sorted so the
// generated SQL is stable and comparable in logs.
func kindsForPool(pool string) []string {
	regMu.RLock()
	defer regMu.RUnlock()
	var out []string
	for kind, r := range registry {
		if r.opts.Pool == pool {
			out = append(out, kind)
		}
	}
	sort.Strings(out)
	return out
}

// nonDBKinds lists every registered kind that does NOT run on the db pool. The
// db pool claims the complement of this set, so an unregistered kind is
// claimed (and immediately failed with `unknown_step_kind`) rather than
// stranded in the queue forever.
func nonDBKinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	var out []string
	for kind, r := range registry {
		if r.opts.Pool != types.StepKindDB {
			out = append(out, kind)
		}
	}
	sort.Strings(out)
	return out
}
