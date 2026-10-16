package services

import (
	"strings"
	"testing"

	"github.com/Arkel-ai/nomos/api/types"
)

// The pièce matchers and the legal-reference scanner, tested without a
// database. A false "found" here is a silently wrong document; a false "not
// found" is a needless warning — both directions are asserted.

func day(y, m, d int) types.PartialDate {
	return types.PartialDate{Precision: types.DatePrecisionDay, Year: y, Month: m, Day: d}
}

func TestDateStatedIn(t *testing.T) {
	cases := []struct {
		name    string
		date    types.PartialDate
		passage string
		want    bool
	}{
		{"long form", day(2025, 3, 3), "Paris, le 3 mars 2025. Monsieur,", true},
		{"premier", day(2025, 3, 1), "Fait le 1er mars 2025", true},
		{"numeric day-first", day(2025, 3, 4), "Date d'envoi : 04/03/2025", true},
		{"numeric is never month-first", day(2025, 4, 3), "Date d'envoi : 04/03/2025", false},
		{"iso", day(2024, 9, 3), "horodatage 2024-09-03T10:00", true},
		{"accent folded, nbsp", day(2025, 2, 12), "le 12 Février 2025", true},
		{"another day", day(2025, 3, 3), "Lettre datée du 5 mars 2025", false},
		{"month does not prove a day", day(2025, 3, 3), "courant mars 2025", false},
		{"month precision", types.PartialDate{Precision: types.DatePrecisionMonth, Year: 2025, Month: 3}, "le 17 mars 2025", true},
		{"month numeric", types.PartialDate{Precision: types.DatePrecisionMonth, Year: 2024, Month: 11}, "bulletin 11/2024", true},
		{"year precision", types.PartialDate{Precision: types.DatePrecisionYear, Year: 2019}, "embauché en 2019", true},
		{"wrong year", types.PartialDate{Precision: types.DatePrecisionYear, Year: 2018}, "embauché en 2019", false},
		{"unknown asserts nothing", types.PartialDate{Precision: types.DatePrecisionUnknown}, "", true},
		{"range opening, two months", day(2025, 3, 20), "La période de mise à pied conservatoire du 20 mars au 7 avril 2025", true},
		{"range closing, two months", day(2025, 4, 7), "du 20 mars au 7 avril 2025", true},
		{"range opening, one month", day(2025, 3, 20), "56 heures de mise à pied du 20 au 31 mars 2025", true},
		{"pair of days", day(2025, 3, 19), "les 19 et 20 mars 2025", true},
		{"range across the new year", day(2024, 12, 20), "du 20 décembre au 5 janvier 2025", true},
		{"range is not every day", day(2025, 3, 25), "du 20 au 31 mars 2025", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DateStatedIn(tc.date, tc.passage); got != tc.want {
				t.Fatalf("DateStatedIn(%+v, %q) = %t, want %t", tc.date, tc.passage, got, tc.want)
			}
		})
	}
}

func TestAmountStatedIn(t *testing.T) {
	eur := func(minor int64, q string) types.Money {
		return types.Money{MinorUnits: minor, Currency: "EUR", Qualifier: q}
	}
	cases := []struct {
		name    string
		m       types.Money
		passage string
		want    bool
	}{
		{"french grouping and decimals", eur(250000, ""), "un salaire de 2 500,00 € par mois", true},
		{"nbsp grouping, euros word", eur(123456, ""), "soit 1 234,56 euros", true},
		{"dot grouping", eur(123456, ""), "montant : 1.234,56 EUR", true},
		{"currency first", eur(30000, ""), "frais de € 300", true},
		{"short decimals", eur(1250, ""), "12,5 €", true},
		{"other value", eur(250000, ""), "un salaire de 2 600,00 €", false},
		{"no currency is no amount", eur(250000, ""), "référence 2500 du dossier", false},
		{"other currency", types.Money{MinorUnits: 250000, Currency: "USD"}, "2 500 €", false},
		{"qualifier stated", eur(250000, types.MoneyGross), "2 500 € bruts mensuels", true},
		{"qualifier absent", eur(250000, types.MoneyNet), "2 500 € bruts mensuels", false},
		{"qualifiers kept apart", eur(195000, types.MoneyNet), "2 500 € brut, soit 1 950 € net", true},
		{"neighbour's qualifier", eur(250000, types.MoneyNet), "2 500 € brut, soit 1 950 € net", false},
		{"ht", eur(100000, types.MoneyHT), "1 000 € HT", true},
		{"table row label", eur(184525, types.MoneyGross), "Total brut                                    1 845,25 €\nCotisations", true},
		{"table row label, net", eur(138555, types.MoneyNet), "– 53,74 €\nsource\nNET PAYÉ                                  1 385,55 €\n", true},
		{"row label of another figure", eur(138555, types.MoneyGross), "NET PAYÉ                                  1 385,55 €", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AmountStatedIn(tc.m, tc.passage); got != tc.want {
				t.Fatalf("AmountStatedIn(%+v, %q) = %t, want %t", tc.m, tc.passage, got, tc.want)
			}
		})
	}
}

func TestCheckPieceClaimsWarnsBesideTheClaim(t *testing.T) {
	ev := []PieceEvidence{{SpanID: "s8", Ordinal: 8, Title: "Lettre de licenciement",
		Passage: "Par lettre du 5 mars 2025, nous vous notifions votre licenciement. Votre salaire de 2 500,00 € brut vous sera versé."}}

	claim := types.DocumentClaim{
		ID: "c1", Kind: types.ClaimFactual, Start: 0, End: 10, EvidenceIDs: []string{"s8"},
		Dates:   []types.ClaimDate{{PartialDate: day(2025, 3, 3), Role: types.DateRoleEvent}},
		Amounts: []types.Money{{MinorUnits: 250000, Currency: "EUR", Qualifier: types.MoneyNet}},
		Quotes:  []types.ClaimQuote{{EvidenceID: "s8", Text: "nous vous notifions votre démission"}},
	}
	issues := CheckPieceClaims("b1", claim, ev)
	codes := map[string]types.VerificationIssue{}
	for _, iss := range issues {
		if iss.Severity != types.SeverityWarning || iss.Check != types.CheckPieceClaims || iss.BlockID != "b1" || iss.ClaimID != "c1" {
			t.Fatalf("pièce issue must be a warning on its block and claim: %+v", iss)
		}
		codes[iss.Code] = iss
	}
	for _, code := range []string{types.IssuePieceDateNotFound, types.IssuePieceAmountMismatch, types.IssuePieceQuoteNotFound} {
		if _, ok := codes[code]; !ok {
			t.Fatalf("missing %s in %+v", code, issues)
		}
	}
	want := "À vérifier — la date du 3 mars 2025 attribuée à la pièce n° 8 n'a pas été retrouvée dans le passage cité."
	if got := codes[types.IssuePieceDateNotFound].Correction; got != want {
		t.Fatalf("date warning = %q, want %q", got, want)
	}

	// The same claim, stated as the passage states it: nothing to say.
	claim.Dates = []types.ClaimDate{{PartialDate: day(2025, 3, 5), Role: types.DateRoleDocument}}
	claim.Amounts = []types.Money{{MinorUnits: 250000, Currency: "EUR", Qualifier: types.MoneyGross}}
	claim.Quotes = []types.ClaimQuote{{EvidenceID: "s8", Text: "nous vous notifions votre licenciement"}}
	if issues := CheckPieceClaims("b1", claim, ev); len(issues) != 0 {
		t.Fatalf("a faithful claim produced issues: %+v", issues)
	}

	// A derived amount needs its calculation, not a figure in the passage.
	derived := types.DocumentClaim{ID: "c2", Kind: types.ClaimDerived, EvidenceIDs: []string{"s8"},
		Amounts: []types.Money{{MinorUnits: 750000, Currency: "EUR"}}}
	if issues := CheckPieceClaims("b2", derived, ev); len(issues) != 1 || issues[0].Code != types.IssuePieceCalculationMissing {
		t.Fatalf("derived amount without calculation: %+v", issues)
	}
	derived.CalculationCallID = "d3.c0"
	if issues := CheckPieceClaims("b2", derived, ev); len(issues) != 0 {
		t.Fatalf("derived amount with calculation: %+v", issues)
	}
}

func TestPieceAttributionNamesEveryPiece(t *testing.T) {
	got := pieceAttribution([]PieceEvidence{{Ordinal: 3}, {Ordinal: 8}, {Ordinal: 3}})
	if got != "aux pièces n° 3 et 8" {
		t.Fatalf("attribution = %q", got)
	}
	if got := pieceAttribution([]PieceEvidence{{Title: "Attestation"}}); got != "à la pièce « Attestation »" {
		t.Fatalf("unnumbered attribution = %q", got)
	}
}

func TestFindLegalReferences(t *testing.T) {
	keys := func(text string) []string {
		var out []string
		for _, r := range FindLegalReferences(text) {
			out = append(out, r.Key)
		}
		return out
	}
	has := func(list []string, k string) bool {
		for _, x := range list {
			if x == k {
				return true
			}
		}
		return false
	}

	if k := keys("En application de l'article L. 1232-1 du code du travail, le licenciement…"); !has(k, "L1232-1") {
		t.Fatalf("code article missed: %v", k)
	}
	if k := keys("voir art. L1234-9 C. trav."); !has(k, "L1234-9") {
		t.Fatalf("compact code article missed: %v", k)
	}
	if k := keys("l'article 1240 du code civil"); !has(k, "1240") {
		t.Fatalf("civil code article missed: %v", k)
	}
	if k := keys("Cass. soc., 12 janvier 2022, n° 21-11.111"); !has(k, "2111111") || !has(k, "date:2022-01-12") {
		t.Fatalf("decision missed: %v", k)
	}
	if k := keys("loi n° 2016-1088 du 8 août 2016"); !has(k, "2016-1088") {
		t.Fatalf("numbered text missed: %v", k)
	}
	if k := keys("ECLI:FR:CCASS:2022:SO00035"); !has(k, "ECLI:FR:CCASS:2022:SO00035") {
		t.Fatalf("ECLI missed: %v", k)
	}
	for _, benign := range []string{
		"l'article 5 du contrat de travail prévoit une clause de mobilité",
		"pièce n° 3, page 2",
		"le 3 mars 2025, le salarié a reçu sa lettre",
	} {
		if k := keys(benign); len(k) != 0 {
			t.Fatalf("%q is not a legal reference, got %v", benign, k)
		}
	}

	// Offsets are code points in the original text, accents included.
	text := "Selon l’arrêt, l'article L. 1232-1 s'applique."
	refs := FindLegalReferences(text)
	if len(refs) != 1 || refs[0].Display != "L. 1232-1" {
		t.Fatalf("display = %+v", refs)
	}
	if got := string([]rune(text)[refs[0].Start:refs[0].End]); got != "L. 1232-1" {
		t.Fatalf("offsets point at %q", got)
	}
}

// The citation forms lawyers actually write must be found, each with the key
// a retrieved source answers to; procedural facts of the dossier must not.
func TestFindLegalReferencesCoversCitationForms(t *testing.T) {
	for text, want := range map[string]string{
		"Selon la chambre sociale (Soc., 25 septembre 2024), ce licenciement est abusif.": "date:2024-09-25",
		"Civ. 1re, 3 mai 2018":                         "date:2018-05-03",
		"Com. 12 juin 2019, la clause est valable":     "date:2019-06-12",
		"Crim., 1er février 2022":                      "date:2022-02-01",
		"la chambre sociale, 25 septembre 2024":        "date:2024-09-25",
		"l'article 1104 du même code":                  "1104",
		"l'article 9 de la loi du 6 juillet 1989":      "9",
		"la loi du 6 juillet 1989 tendant à améliorer": "loi:1989-07-06",
		"CA Paris, 12 mars 2023":                       "date:2023-03-12",
		"cour d'appel de Lyon, 3 mai 2021":             "date:2021-05-03",
		"CE, 10 mars 2020, n° 432123":                  "date:2020-03-10",
		"CE, ass., 10 mars 2020":                       "date:2020-03-10",
		"l'article 8 de la CEDH":                       "8",
		"CEDH, 5 sept. 2017, Bărbulescu":               "date:2017-09-05",
		"CJUE, 9 nov. 2023":                            "date:2023-11-09",
		"Cons. const., déc. n° 2018-761 QPC":           "2018761",
		"Conseil constitutionnel, 21 mars 2018":        "date:2018-03-21",
	} {
		found := false
		for _, r := range FindLegalReferences(text) {
			found = found || r.Key == want
		}
		if !found {
			t.Fatalf("%q: key %q not found in %+v", text, want, FindLegalReferences(text))
		}
	}
	for _, benign := range []string{
		"ça a été notifié, le 3 mars 2025",
		"ce 10 mars 2020, le salarié a été convoqué",
		"l'appel interjeté devant la cour d'appel de Lyon le 3 mars 2025",
		"l'ordonnance du 3 mars 2025 rendue par le conseiller de la mise en état",
		"l'article 2 du règlement intérieur",
	} {
		if refs := FindLegalReferences(benign); len(refs) != 0 {
			t.Fatalf("%q is not a legal reference, got %+v", benign, refs)
		}
	}
	// One reference written two ways is reported once.
	if refs := FindLegalReferences("Cass. soc., 25 septembre 2024"); len(refs) != 1 {
		t.Fatalf("duplicate references: %+v", refs)
	}
	// A dated text's key is answered by its stored label.
	keys := LegalSourceKeys(AgentLegalSource{Document: types.LegalDocument{
		CitationLabel: "Loi n° 89-462 du 6 juillet 1989 tendant à améliorer les rapports locatifs, art. 9"}})
	for _, r := range FindLegalReferences("l'article 9 de la loi du 6 juillet 1989") {
		if !keys[r.Key] {
			t.Fatalf("prose key %q not answered by the label keys %v", r.Key, keys)
		}
	}
}

// Dates and amounts are found where they are printed, at code-point offsets,
// a full date counting once and a year alone not at all.
func TestFindTextMentions(t *testing.T) {
	text := "Notifié le 14 mars 2025 ; indemnité de 9 999,00 € brut ; échéance 03/2026 ; loi de 2016."
	ms := FindTextMentions(text)
	if len(ms) != 3 {
		t.Fatalf("mentions = %+v", ms)
	}
	if d := ms[0].Date; d == nil || *d != day(2025, 3, 14) || string([]rune(text)[ms[0].Start:ms[0].End]) != "14 mars 2025" {
		t.Fatalf("date mention = %+v", ms[0])
	}
	if m := ms[1].Money; m == nil || m.MinorUnits != 999900 || m.Currency != "EUR" || m.Qualifier != types.MoneyGross {
		t.Fatalf("amount mention = %+v", ms[1])
	}
	if d := ms[2].Date; d == nil || d.Precision != types.DatePrecisionMonth || d.Month != 3 || d.Year != 2026 {
		t.Fatalf("month mention = %+v", ms[2])
	}
}

func TestLegalSourceKeysMatchProse(t *testing.T) {
	src := AgentLegalSource{
		Document: types.LegalDocument{CitationLabel: "C. trav., art. L. 1232-1"},
		Version:  types.LegalVersion{SourceMeta: types.JSONB(`{"article_number":"L1232-1"}`)},
	}
	keys := LegalSourceKeys(src)
	for _, r := range FindLegalReferences("l'article L.1232-1 du code du travail") {
		if !keys[r.Key] {
			t.Fatalf("prose key %q not answered by the source keys %v", r.Key, keys)
		}
	}
}

func TestLocateQuoteMapsBackToOriginalOffsets(t *testing.T) {
	source := "Le contrat prend effet le 1er avril.  L’employeur  s’engage à verser…"
	quote := "l'employeur s'engage"
	start, end, ok := LocateQuote(source, quote)
	if !ok {
		t.Fatal("quote not located")
	}
	if got := string([]rune(source)[start:end]); got != "L’employeur  s’engage" {
		t.Fatalf("located %q", got)
	}
	if _, _, ok := LocateQuote(source, "l'employeur refuse"); ok {
		t.Fatal("a paraphrase must not be located")
	}
	// The offset-keeping normalisation is the quote-matching one.
	for _, s := range []string{source, "  a­ b «c» — d’e  ", ""} {
		norm, _ := normalizeWithOffsets(s)
		if string(norm) != normalizeForQuoteMatch(s) {
			t.Fatalf("normalisations diverge on %q: %q vs %q", s, string(norm), normalizeForQuoteMatch(s))
		}
	}
	if !strings.Contains(FrenchMoney(types.Money{MinorUnits: 123456789, Currency: "EUR", Qualifier: types.MoneyGross}), "1 234 567,89 € brut") {
		t.Fatalf("money rendering: %q", FrenchMoney(types.Money{MinorUnits: 123456789, Currency: "EUR"}))
	}
}
