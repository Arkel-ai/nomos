// Live smoke test against the real PISTE gateway. SKIPPED unless PISTE_LIVE=1
// or NOMOS_LIVE=1, and always excluded under NOMOS_LIVE=0, because the rest of
// this package is deliberately offline (httptest servers, no credentials) and
// CI has neither network nor a client secret. Requested with
// NOMOS_REQUIRE_DEPENDENCIES=1 and no credentials, it fails instead of skipping.
//
// It answers one question that recorded JSON cannot: are the credentials in
// api/.env actually entitled to these two APIs? A valid token proves the
// application exists; only a real call proves the CGU were accepted and the
// APIs attached to it (a 403 with an empty body is exactly that failure).
//
//	set -a && . ./.env && set +a && PISTE_LIVE=1 go test ./connectors -run Live -v
package connectors_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Arkel-ai/nomos/api/connectors"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
)

func liveOptions(t *testing.T, baseURL string) connectors.Options {
	t.Helper()
	testmode.Live(t, "PISTE live smoke", "PISTE_LIVE")
	id, secret := os.Getenv("PISTE_CLIENT_ID"), os.Getenv("PISTE_CLIENT_SECRET")
	if id == "" || secret == "" {
		testmode.Missing(t, "PISTE_CLIENT_ID / PISTE_CLIENT_SECRET unset")
	}
	return connectors.Options{
		ClientID:     id,
		ClientSecret: secret,
		TokenURL:     os.Getenv("PISTE_TOKEN_URL"),
		BaseURL:      baseURL,
	}
}

func TestLiveLegifranceSearch(t *testing.T) {
	opts := liveOptions(t, os.Getenv("LEGIFRANCE_BASE_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := connectors.NewLegifrance(opts).Search(ctx, connectors.SearchQuery{
		Fond:     "CODE_DATE",
		Terms:    "licenciement",
		CodeName: "Code du travail",
		PageSize: 3,
	})
	if err != nil {
		t.Fatalf("Légifrance search: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatalf("Légifrance search returned no articles (total_texts=%d)", res.Total)
	}
	// A non-empty page is not the assertion that matters: the hit has to
	// carry the LEGIARTI id, or the loader has nothing to call GetArticle
	// with. Decoding the envelope at the wrong depth returns hits that look
	// fine by count and are empty by content.
	for _, r := range res.Results {
		if r.ID == "" || r.Num == "" {
			t.Fatalf("hit missing the identity GetArticle needs: %+v", r)
		}
	}
	first := res.Results[0]
	t.Logf("Légifrance OK: %d articles (total_texts=%d) first=%s %s [%s] in %q",
		len(res.Results), res.Total, first.ID, first.Num, first.Etat, first.TextTitle)

	art, err := connectors.NewLegifrance(opts).GetArticle(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetArticle(%s): %v", first.ID, err)
	}
	if art.Text == "" {
		t.Fatalf("GetArticle(%s) returned empty text: %+v", first.ID, art)
	}
	from, to := art.Validity()
	t.Logf("GetArticle OK: %s num=%s etat=%s len=%d valid_from=%v valid_to=%v",
		art.ID, art.Num, art.Etat, len(art.Text), from, to)
}

func TestLiveJudilibreSearch(t *testing.T) {
	opts := liveOptions(t, os.Getenv("JUDILIBRE_BASE_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	q := connectors.EmploymentScope()
	q.Query = "licenciement sans cause réelle et sérieuse"
	q.PageSize = 3

	res, err := connectors.NewJudilibre(opts).Search(ctx, q)
	if err != nil {
		t.Fatalf("Judilibre search: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatalf("Judilibre search returned no results (total=%d)", res.Total)
	}
	t.Logf("Judilibre OK: total=%d first=%s %s", res.Total, res.Results[0].ID, res.Results[0].DecisionDate)
}

// Proves the loader's scope assumption still holds against the live code: one
// /consult/legiPart call returns the whole Code du travail with every article
// body inline, so the loader needs no per-article round trip.
func TestLiveLegifranceLegiPart(t *testing.T) {
	opts := liveOptions(t, os.Getenv("LEGIFRANCE_BASE_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	tree, err := connectors.NewLegifrance(opts).LegiPart(ctx, "LEGITEXT000006072050", time.Time{})
	if err != nil {
		t.Fatalf("LegiPart: %v", err)
	}
	arts := tree.FlatArticles()
	if len(arts) < 5000 {
		t.Fatalf("walked only %d articles — the tree shape probably changed", len(arts))
	}
	inForce, withText := 0, 0
	for _, a := range arts {
		if a.Etat != "VIGUEUR" {
			continue
		}
		inForce++
		if a.Text() != "" {
			withText++
		}
	}
	if withText != inForce {
		t.Fatalf("%d of %d in-force articles have an empty body; the loader would skip them",
			inForce-withText, inForce)
	}
	t.Logf("legiPart OK: revision=%s modified=%s articles=%d in_force=%d all_with_text=%t",
		tree.ID, tree.ModifDate, len(arts), inForce, withText == inForce)
}
