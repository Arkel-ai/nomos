package services

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// THE CORPUS CLAIM IS A LEASE (§2.1, §4.2).
//
// SeedCorpus promises that "a pod killed mid-load leaves a cursor, and the
// next boot continues from it". That promise only holds if the claim the dead
// pod took can be taken away from it: finishSync runs inside the pod, so a pod
// that is OOMKilled releases nothing. Tested against the real Postgres,
// because the claim IS a conditional UPDATE.
//
// The regression locked down here was live in the cluster: an OOMKilled loader
// left corpus_syncs at state='running', and every boot afterwards logged
// "another worker holds the sync" and seeded nothing, forever.

// seedClaim writes a corpus_syncs row already held by a worker whose last sign
// of life was lastProgress ago.
func seedClaim(t *testing.T, lastProgress time.Duration) string {
	t.Helper()
	openTestDB(t)

	source := "test-source-" + uuid.NewString()[:8]
	last := time.Now().Add(-lastProgress).Unix()
	row := types.CorpusSync{
		ID:        source,
		Source:    source,
		State:     types.CorpusSyncRunning,
		Cursor:    "128",
		LastRunAt: &last,
	}
	if err := u.DB.Create(&row).Error; err != nil {
		t.Fatalf("seeding the claim: %v", err)
	}
	t.Cleanup(func() { u.DB.Exec(`DELETE FROM corpus_syncs WHERE source = ?`, source) })
	return source
}

func syncRow(t *testing.T, source string) types.CorpusSync {
	t.Helper()
	var row types.CorpusSync
	if err := u.DB.Where("source = ?", source).First(&row).Error; err != nil {
		t.Fatalf("reading corpus_syncs (source=%s): %v", source, err)
	}
	return row
}

// A worker that died mid-load holds the row at `running` and will never
// release it. The next claimer has to take it, and has to resume from the
// cursor the dead worker left rather than restart the source.
func TestClaimSyncOvertakesADeadWorker(t *testing.T) {
	source := seedClaim(t, 2*syncLease)

	claimed, won, err := claimSync(context.Background(), source, false)
	if err != nil {
		t.Fatalf("claimSync: %v", err)
	}
	if !won {
		t.Fatal("a claim untouched for twice the lease was not overtaken — the row stays 'running' forever and the source never seeds again")
	}
	if claimed.Cursor != "128" {
		t.Fatalf("cursor after the takeover = %q, want %q — the takeover restarted the load instead of resuming it", claimed.Cursor, "128")
	}

	// The takeover re-stamps liveness, so a third worker arriving right behind
	// it sees a live claim.
	if _, won, err := claimSync(context.Background(), source, false); err != nil || won {
		t.Fatalf("a second claimer won=%t (err=%v) right after the takeover — the claim did not refresh last_run_at", won, err)
	}
}

// The other half: a claim that is still moving is never stolen. Progress is
// the only thing separating the two cases, which is what keeps syncLease
// honest.
func TestClaimSyncDoesNotOvertakeALiveRun(t *testing.T) {
	source := seedClaim(t, syncLease/2)

	_, won, err := claimSync(context.Background(), source, false)
	if err != nil {
		t.Fatalf("claimSync: %v", err)
	}
	if won {
		t.Fatal("a live run was overtaken — two loaders would walk the same source at once")
	}
	if got := syncRow(t, source); got.State != types.CorpusSyncRunning || got.Cursor != "128" {
		t.Fatalf("the live row was disturbed: state=%q cursor=%q", got.State, got.Cursor)
	}
}

// A batch write IS the heartbeat: a slow-but-alive loader that keeps
// persisting progress keeps its claim, however long the whole run takes.
func TestPersistProgressRenewsTheClaim(t *testing.T) {
	source := seedClaim(t, 2*syncLease)

	persistProgress(context.Background(), source, "160", loadStats{Articles: 32})

	_, won, err := claimSync(context.Background(), source, false)
	if err != nil {
		t.Fatalf("claimSync: %v", err)
	}
	if won {
		t.Fatal("a loader that had just written a batch was overtaken — persistProgress does not stamp last_run_at")
	}
	if got := syncRow(t, source); got.Cursor != "160" {
		t.Fatalf("cursor after the progress write = %q, want %q", got.Cursor, "160")
	}
}
