package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/types"
)

// The public-query deny-list (owner decision 2): every category must fire on
// what it is for and stay quiet on ordinary legal research.

func patternCategories(q string) []string {
	var v PublicQueryVerdict
	checkPublicPatterns(q, &v)
	return sortedCategories(v)
}

func TestPublicPatternsFireOnIdentifyingData(t *testing.T) {
	cases := map[string]string{
		"écrire à jean.dupont@exemple.fr":            PublicQueryEmail,
		"rappeler au 06 12 34 56 78":                 PublicQueryPhone,
		"standard +33 1 23 45 67 89":                 PublicQueryPhone,
		"virement FR76 3000 6000 0112 3456 7890 189": PublicQueryIBAN,
		"établissement SIRET 732 829 320 00074":      PublicQuerySIRET,
		"société immatriculée 732 829 320":           PublicQuerySIREN,
		"assuré 1 85 05 78 006 084 36":               PublicQueryNIR,
		"domicilié 12 bis rue de la Paix":            PublicQueryAddress,
		"siège à 75008 Paris":                        PublicQueryAddress,
		"adresse BP 123":                             PublicQueryAddress,
		"rappel de salaire de 1 500 €":               PublicQueryAmount,
		"prime de 2450,75 euros":                     PublicQueryAmount,
		"indemnité €300":                             PublicQueryAmount,
	}
	for q, want := range cases {
		got := patternCategories(q)
		if len(got) == 0 || !hasCategory(got, want) {
			t.Fatalf("%q: want %s, got %v", q, want, got)
		}
	}
}

func TestPublicPatternsLeaveLegalResearchAlone(t *testing.T) {
	for _, q := range []string{
		"article L1234-1 du code du travail préavis",
		"Cass. soc. 12 janvier 2022 n° 21-11.111 faute grave",
		"loi n° 2016-1088 du 8 août 2016 dite loi travail",
		"délai de prescription de l'action en paiement des salaires",
		"indemnité légale de licenciement 1/4 de mois par année d'ancienneté",
		"article 1240 du code civil responsabilité",
		"décret 2008-244 recodification",
		"convention collective nationale des transports routiers",
	} {
		if got := patternCategories(q); len(got) > 0 {
			t.Fatalf("%q must pass, got %v", q, got)
		}
	}
}

func testDenyList() *publicDenyList {
	d := &publicDenyList{names: map[string]bool{}, nameTokens: map[string]bool{}, titleTokens: map[string]bool{}, dates: map[string]bool{}}
	d.addName("M. Jean-Luc MOREAU")
	d.addName("SAS Transports Michelet")
	d.addName("URSSAF Île-de-France")
	d.addTitle("Moreau c/ Michelet — RG 21/01234")
	d.dates["2024-03-12"] = true
	d.texts = []string{"Le salarié a refusé la mutation proposée par courrier recommandé du 12 mars 2024."}
	return d
}

func denyCategories(d *publicDenyList, q string) []string {
	var v PublicQueryVerdict
	d.check(q, &v)
	return sortedCategories(v)
}

func TestDenyListCategories(t *testing.T) {
	d := testDenyList()
	positive := map[string]string{
		"licenciement de jean-luc moreau":                       PublicQueryPartyName,
		"Transports MICHELET faute inexcusable":                 PublicQueryPartyName,
		"dossier RG 01234":                                      PublicQueryDossierTitle,
		"mutation refusée le 12 mars 2024":                      PublicQueryPieceDate,
		"courrier du 12/03/2024 préavis":                        PublicQueryPieceDate,
		"a refusé la mutation proposée par courrier recommandé": PublicQueryPieceText,
	}
	for q, want := range positive {
		if got := denyCategories(d, q); !hasCategory(got, want) {
			t.Fatalf("%q: want %s, got %v", q, want, got)
		}
	}
	for _, q := range []string{
		"refus d'une mutation et clause de mobilité",
		"transports routiers convention collective",
		"urssaf redressement cotisations",
		"loi du 12 mars 2024 relative au travail",
		"pourvoi 21-11.111 chambre sociale",
		"Cass. soc. 12 janvier 2022 faute grave",
	} {
		if got := denyCategories(d, q); len(got) > 0 {
			t.Fatalf("%q must pass, got %v", q, got)
		}
	}
}

// The gaps a deny-list built only from qualified parties left open: a client
// whose name starts like a public body, a three-letter surname, what the
// lawyer typed, and a dossier too large to scan.
func TestDenyListClosesItsGaps(t *testing.T) {
	d := &publicDenyList{names: map[string]bool{}, nameTokens: map[string]bool{}, titleTokens: map[string]bool{}, dates: map[string]bool{}}
	d.addName("CAF Conseil Brindille")
	d.addName("Etat Major Zorglub SAS")
	d.addName("Jean Roy")
	d.addName("URSSAF")
	d.addName("Caisse d'allocations familiales")
	d.addLawyerText("M. Kowalczyk, licencié par Transmontagne le 14 mars 2025, conteste la rupture. Contestation urgente.")
	for _, q := range []string{
		"licenciement salarié Brindille",
		"Zorglub harcèlement moral",
		"licenciement Roy faute grave",
		"licenciement Kowalczyk transporteur",
		"Transmontagne faute grave",
		"licenciement notifié le 14 mars 2025",
	} {
		if got := denyCategories(d, q); len(got) == 0 {
			t.Fatalf("%q must be refused", q)
		}
	}
	for _, q := range []string{
		"urssaf redressement cotisations",
		"caf allocations familiales prestations",
		"contestation du licenciement délai de prescription",
		"loi du 14 mars 2025 relative au travail",
	} {
		if got := denyCategories(d, q); len(got) > 0 {
			t.Fatalf("%q must pass, got %v", q, got)
		}
	}
	d.bounded = true
	if got := denyCategories(d, "faute grave préavis"); !hasCategory(got, PublicQueryUncheckable) {
		t.Fatalf("an unscanned dossier must refuse every query, got %v", got)
	}
}

func TestCheckPublicQueryReadsTheTenantAndDossier(t *testing.T) {
	f := newToolFixture(t)
	date := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC).Unix()
	p := f.addPiece(pieceSpec{ordinal: 1, name: "lettre.pdf", mime: mimePDF, pages: []string{"Lettre de licenciement adressée à Madame Lefebvre-Sauvage."},
		docDate: &date, datePrec: types.DatePrecisionDay})
	f.addChunk(p, 0, "Lettre de licenciement adressée à Madame Lefebvre-Sauvage pour motif économique et réorganisation de l'entreprise.")
	party := types.PiecePartie{ID: uuid.NewString(), PieceID: p.ID, Role: types.PartyRoleRecipient,
		DisplayName: "Madame Lefebvre-Sauvage", NormalizedName: NormalizePartyName("Madame Lefebvre-Sauvage")}
	if err := f.repo().Create(&party); err != nil {
		t.Fatal(err)
	}
	unindexed := f.addPiece(pieceSpec{ordinal: 2, name: "note.pdf", mime: mimePDF,
		pages: []string{"Le comité social et économique a rendu un avis défavorable unanime sur le projet."}})
	_ = unindexed

	ctx := context.Background()
	check := func(dossier, q string) []string {
		v, err := CheckPublicQuery(ctx, f.repo(), dossier, q)
		if err != nil {
			t.Fatalf("CheckPublicQuery: %v", err)
		}
		return sortedCategories(v)
	}
	if got := check(f.dossier.ID, "licenciement lefebvre motif économique"); !hasCategory(got, PublicQueryPartyName) {
		t.Fatalf("party name: %v", got)
	}
	if got := check(f.dossier.ID, "lettre du 15 janvier 2025 motif"); !hasCategory(got, PublicQueryPieceDate) {
		t.Fatalf("piece document date: %v", got)
	}
	if got := check(f.dossier.ID, "motif économique et réorganisation de l'entreprise"); !hasCategory(got, PublicQueryPieceText) {
		t.Fatalf("copied run: %v", got)
	}
	// A piece not yet indexed is read from its sidecar.
	if got := check(f.dossier.ID, "a rendu un avis défavorable unanime sur"); !hasCategory(got, PublicQueryPieceText) {
		t.Fatalf("unindexed piece text: %v", got)
	}
	if got := check(f.dossier.ID, "michelet préavis"); !hasCategory(got, PublicQueryDossierTitle) {
		t.Fatalf("dossier title: %v", got)
	}
	// Without a dossier, the tenant's party names still apply.
	if got := check("", "jurisprudence lefebvre-sauvage"); !hasCategory(got, PublicQueryPartyName) {
		t.Fatalf("tenant-wide names: %v", got)
	}
	if got := check(f.dossier.ID, "licenciement pour motif économique critères d'ordre"); len(got) > 0 {
		t.Fatalf("an ordinary legal query must pass: %v", got)
	}
	if got := check(f.dossier.ID, "ligne 1\nligne 2"); !hasCategory(got, PublicQueryShape) {
		t.Fatalf("multi-line query: %v", got)
	}
}

func hasCategory(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// The query-shape gate: a question of law is short and has no line breaks,
// dossier prose is long and has them. It must reject on shape, never on trust
// in the prompt that produced the query.
func TestWebQueryGateRejectsDossierProse(t *testing.T) {
	cases := []struct {
		name  string
		query string
		ok    bool
	}{
		{"question of law", "responsabilité du fait des produits défectueux : délai de prescription", true},
		{"empty", "   ", false},
		{"multiline is prose, not a question", "M. Dupont a livré\nle 3 mars 2019", false},
		{"too long to be anything but an extract", strings.Repeat("a", types.MaxWebQueryRunes+1), false},
		{"exactly at the bound", strings.Repeat("a", types.MaxWebQueryRunes), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := webQueryIsSafe(tc.query)
			if tc.ok && err != nil {
				t.Fatalf("query rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("query accepted — it must not leave the cabinet")
			}
		})
	}
}
