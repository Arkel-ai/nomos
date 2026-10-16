package services

import (
	"testing"

	"github.com/Arkel-ai/nomos/api/connectors"
)

// art builds a CodeArticle with just the fields scoping and ordering read.
func art(num, id, etat string) connectors.CodeArticle {
	return connectors.CodeArticle{ID: id, CID: "C" + id, Num: num, Etat: etat, Content: "<p>x</p>"}
}

func TestScopeToEmploymentKeepsOnlyInForceFamilies(t *testing.T) {
	in := []connectors.CodeArticle{
		art("L1232-1", "A1", "VIGUEUR"),  // in scope
		art("L8241-1", "A2", "VIGUEUR"),  // family not in the employment scope
		art("L1232-6", "A3", "ABROGE"),   // in family, not in force
		art("R1234-1", "A4", "VIGUEUR"),  // in scope
		art("L 1233-3", "A5", "VIGUEUR"), // in scope once the space is normalised
	}
	got := scopeToEmployment(in)

	if len(got) != 3 {
		t.Fatalf("kept %d articles, want 3: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Num == "L8241-1" || a.Etat == "ABROGE" {
			t.Fatalf("out-of-scope article survived: %+v", a)
		}
	}
	// "L 1233-3" must be normalised, or the family prefix never matches and
	// the article silently disappears from the corpus.
	var found bool
	for _, a := range got {
		if a.Num == "L1233-3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("space-padded article number was not normalised: %+v", got)
	}
}

func TestScopeToEmploymentOrderIsTotalAndSorted(t *testing.T) {
	got := scopeToEmployment([]connectors.CodeArticle{
		art("L1232-24", "B", "VIGUEUR"),
		art("L1232-2", "A", "VIGUEUR"),
		art("L1232-2", "Z", "VIGUEUR"),
	})
	want := []string{"L1232-2#A", "L1232-2#Z", "L1232-24#B"}
	for i, a := range got {
		if articleCursor(a) != want[i] {
			t.Fatalf("position %d is %q, want %q (full order: %+v)", i, articleCursor(a), want[i], got)
		}
	}
}

// Ten working-time provisions hang under two headings each after the 2016
// restructuring, so the tree yields the SAME article (identical id and cid)
// twice. Undeduplicated, the batch holding one becomes an INSERT with two rows
// sharing a conflict key, and Postgres rejects the statement outright:
// "ON CONFLICT DO UPDATE command cannot affect row a second time".
func TestScopeToEmploymentDropsTheSameArticleSeenTwice(t *testing.T) {
	same := art("L3121-24", "LEGIARTI000033020389", "VIGUEUR")
	got := scopeToEmployment([]connectors.CodeArticle{same, same})

	if len(got) != 1 {
		t.Fatalf("kept %d copies of one article, want 1: %+v", len(got), got)
	}
}

// The cursor must stay total even when two DISTINCT articles share a number,
// or a batch boundary between them drops one on every resumed run.
func TestArticleCursorIsTotalAcrossASharedNumber(t *testing.T) {
	first := art("L3121-24", "LEGIARTI000033020424", "VIGUEUR")
	other := art("L3121-24", "LEGIARTI000042683474", "VIGUEUR")

	if articleCursor(first) == articleCursor(other) {
		t.Fatalf("cursor is not total: both articles map to %q", articleCursor(first))
	}
	if !(articleCursor(other) > articleCursor(first)) {
		t.Fatalf("resuming past %q would skip %q", articleCursor(first), articleCursor(other))
	}
}

func TestRetrievalTextForLeadsWithCitationAndPath(t *testing.T) {
	got := retrievalTextFor("C. trav., art. L1232-1", []string{"Livre Ier", "Titre III"}, "Le texte.")
	want := "C. trav., art. L1232-1 — Livre Ier › Titre III\n\nLe texte."
	if got != want {
		t.Fatalf("retrievalTextFor:\n got %q\nwant %q", got, want)
	}
}
