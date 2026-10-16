package services

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/Arkel-ai/nomos/api/types"
)

// Pièce checks. A claim attributed to a pièce is
// checked against the exact passage the agent read (the evidence span's frozen
// quote, the text at its locator): quotations must occur in it, dates must be
// stated in it at a compatible precision, amounts must match value, currency
// and qualifier. A mismatch is reported at that locator when it is finer than
// a page — « § 4 » of a DOCX, « 'Chiffrage des demandes'!D8 » of a workbook.
//
// A mismatch is a WARNING, never a block: pièces are trusted sources and the
// check is operational, not an authenticity judgement. The warning is written
// once, here, as the « À vérifier — … » sentence that travels unchanged into
// the report (VerificationIssue.Correction), the answer and the DOCX. Nothing
// here corrects a claim to the closest value: the agent repairs it, or the
// warning stays beside it.
//
// Every function in this file is pure — no database, no I/O — so the matching
// rules are tested directly (piece_verification_test.go).

// PieceEvidence is one pièce passage a claim cites, with the pièce's display
// identity for the warning sentence.
type PieceEvidence struct {
	SpanID  string
	Passage string // the evidence span's frozen quote: the text at Locator
	Ordinal int
	Title   string
	// Locator is where the passage sits. A mismatch is reported at it when
	// it names a place finer than a page: a PDF paragraph, a DOCX paragraph,
	// an XLSX cell.
	Locator types.SourceLocator
}

// pieceEvidenceOf is the verifier's view of one pièce span.
func pieceEvidenceOf(spanID string, span types.AgentEvidenceSpan, p types.Piece) PieceEvidence {
	ev := PieceEvidence{SpanID: spanID, Passage: span.Quote, Ordinal: p.Ordinal, Title: pieceTitle(p)}
	_ = json.Unmarshal(span.Locator, &ev.Locator) // a span without a locator is reported at the pièce
	return ev
}

// CheckPieceClaims checks one claim's pièce-attributed quotes, dates and
// amounts. evidence holds the pièce spans the claim cites; a claim citing no
// pièce has nothing to check here.
func CheckPieceClaims(blockID string, claim types.DocumentClaim, evidence []PieceEvidence) []types.VerificationIssue {
	if len(evidence) == 0 {
		return nil
	}
	var out []types.VerificationIssue
	byID := make(map[string]PieceEvidence, len(evidence))
	spanIDs := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		byID[ev.SpanID] = ev
		spanIDs = append(spanIDs, ev.SpanID)
	}
	warn := func(code string, ids []string, sentence string) {
		out = append(out, types.VerificationIssue{
			Code:        code,
			Severity:    types.SeverityWarning,
			Check:       types.CheckPieceClaims,
			BlockID:     blockID,
			ClaimID:     claim.ID,
			EvidenceIDs: ids,
			Correction:  sentence,
		})
	}

	for _, q := range claim.Quotes {
		ev, ok := byID[q.EvidenceID]
		if !ok {
			continue // a quote on a legal or web span is checked by its own check
		}
		if !QuoteOccursIn(q.Text, ev.Passage) {
			warn(types.IssuePieceQuoteNotFound, []string{ev.SpanID}, fmt.Sprintf(
				"À vérifier — la citation attribuée %s n'a pas été retrouvée dans le passage cité.",
				pieceAttribution([]PieceEvidence{ev})))
		}
	}

	passages := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		passages = append(passages, ev.Passage)
	}

	for _, d := range claim.Dates {
		if d.Precision == types.DatePrecisionUnknown || d.Precision == "" {
			continue
		}
		if !DateStatedIn(d.PartialDate, passages...) {
			warn(types.IssuePieceDateNotFound, spanIDs, fmt.Sprintf(
				"À vérifier — la date du %s attribuée %s n'a pas été retrouvée dans %s.",
				FrenchPartialDate(d.PartialDate), pieceAttribution(evidence), passagePhrase(evidence)))
		}
	}

	if claim.Kind == types.ClaimDerived {
		if len(claim.Amounts) > 0 && strings.TrimSpace(claim.CalculationCallID) == "" {
			warn(types.IssuePieceCalculationMissing, spanIDs,
				"À vérifier — ce montant calculé ne s'appuie sur aucun calcul enregistré.")
		}
		return out
	}
	for _, m := range claim.Amounts {
		if !AmountStatedIn(m, passages...) {
			warn(types.IssuePieceAmountMismatch, spanIDs, fmt.Sprintf(
				"À vérifier — le montant de %s attribué %s n'a pas été retrouvé dans %s.",
				FrenchMoney(m), pieceAttribution(evidence), passagePhrase(evidence)))
		}
	}
	return out
}

// Warning sentences for claims that rest on no pièce passage at all.
const (
	warnUnsupportedFact = "À vérifier — ce fait n'est rattaché à aucun passage de pièce lu."
	warnLawyerProvided  = "À vérifier — ce fait est présenté comme fourni par l'avocat mais ne figure ni dans ses messages ni dans ses réponses."
)

// pieceAttribution is "à la pièce n° 8" / "aux pièces n° 3 et 8", each pièce
// followed by the places cited in it when they are finer than a page: "à la
// pièce n° 6 (§ 4)", "à la pièce n° 18 ('Chiffrage des demandes'!D8)".
func pieceAttribution(evidence []PieceEvidence) string {
	type cited struct {
		ev     PieceEvidence
		places []string
	}
	var refs []*cited
	byKey := map[string]*cited{}
	for _, ev := range evidence {
		key := fmt.Sprintf("%d|%s", ev.Ordinal, ev.Title)
		r, ok := byKey[key]
		if !ok {
			r = &cited{ev: ev}
			byKey[key] = r
			refs = append(refs, r)
		}
		if at := fineLocatorText(ev.Locator); at != "" && !containsString(r.places, at) {
			r.places = append(r.places, at)
		}
	}
	var nums []string
	var named []string
	for _, r := range refs {
		where := ""
		if len(r.places) > 0 {
			where = " (" + strings.Join(r.places, ", ") + ")"
		}
		if r.ev.Ordinal > 0 {
			nums = append(nums, strconv.Itoa(r.ev.Ordinal)+where)
		} else {
			named = append(named, "« "+firstNonEmpty(strings.TrimSpace(r.ev.Title), "sans titre")+" »"+where)
		}
	}
	var parts []string
	if len(nums) > 0 {
		parts = append(parts, "n° "+joinFrench(nums))
	}
	parts = append(parts, named...)
	if len(nums)+len(named) > 1 {
		return "aux pièces " + joinFrench(parts)
	}
	return "à la pièce " + joinFrench(parts)
}

// fineLocatorText names a place finer than a page — a PDF paragraph, a DOCX
// paragraph, an XLSX cell — and nothing for a page alone: the citation itself
// already carries the page.
func fineLocatorText(loc types.SourceLocator) string {
	switch {
	case loc.Kind == types.LocatorPDF && loc.Paragraph > 0,
		loc.Kind == types.LocatorDOCX && loc.Paragraph > 0,
		loc.Kind == types.LocatorXLSX && loc.Cell != "":
		return pieceLocatorText(loc)
	}
	return ""
}

func passagePhrase(evidence []PieceEvidence) string {
	if len(evidence) > 1 {
		return "les passages cités"
	}
	return "le passage cité"
}

func joinFrench(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " et " + items[len(items)-1]
}

// ---------------------------------------------------------------------------
// Display
// ---------------------------------------------------------------------------

// FrenchPartialDate renders a date at its own precision: "3 mars 2025",
// "mars 2025", "2025" — never a manufactured day.
func FrenchPartialDate(d types.PartialDate) string {
	switch d.Precision {
	case types.DatePrecisionDay:
		if d.Month < 1 || d.Month > 12 {
			break
		}
		day := strconv.Itoa(d.Day)
		if d.Day == 1 {
			day = "1er"
		}
		return fmt.Sprintf("%s %s %d", day, frenchMonths[d.Month-1], d.Year)
	case types.DatePrecisionMonth:
		if d.Month < 1 || d.Month > 12 {
			break
		}
		return fmt.Sprintf("%s %d", frenchMonths[d.Month-1], d.Year)
	case types.DatePrecisionYear:
		return strconv.Itoa(d.Year)
	}
	return "date non établie"
}

// FrenchMoney renders minor units as "2 500,00 €" (or "2 500,00 USD").
func FrenchMoney(m types.Money) string {
	neg := m.MinorUnits < 0
	v := m.MinorUnits
	if neg {
		v = -v
	}
	units := strconv.FormatInt(v/100, 10)
	var grouped strings.Builder
	for i, r := range units {
		if i > 0 && (len(units)-i)%3 == 0 {
			grouped.WriteByte(' ')
		}
		grouped.WriteRune(r)
	}
	s := fmt.Sprintf("%s,%02d", grouped.String(), v%100)
	if neg {
		s = "-" + s
	}
	cur := m.Currency
	if cur == "EUR" {
		cur = "€"
	}
	s += " " + cur
	switch m.Qualifier {
	case types.MoneyGross:
		s += " brut"
	case types.MoneyNet:
		s += " net"
	case types.MoneyHT:
		s += " HT"
	case types.MoneyTTC:
		s += " TTC"
	}
	return s
}

// ---------------------------------------------------------------------------
// Folding
// ---------------------------------------------------------------------------

// foldForMatch lowercases, strips French diacritics and flattens the spaces a
// PDF text layer invents, one rune for one rune — so a regexp match offset on
// the folded text is the same code-point offset in the original.
func foldForMatch(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

func foldRune(r rune) rune {
	switch r {
	case ' ', ' ', ' ', ' ', '\t', '\n', '\r':
		return ' '
	case '’', 'ʼ', '‘':
		return '\''
	case '‐', '‑', '‒', '–', '—':
		return '-'
	}
	r = unicode.ToLower(r)
	switch r {
	case 'à', 'â', 'ä', 'á', 'æ':
		return 'a'
	case 'ç':
		return 'c'
	case 'é', 'è', 'ê', 'ë':
		return 'e'
	case 'î', 'ï', 'í':
		return 'i'
	case 'ô', 'ö', 'ó', 'œ':
		return 'o'
	case 'ù', 'û', 'ü', 'ú':
		return 'u'
	case 'ÿ':
		return 'y'
	}
	return r
}

// ---------------------------------------------------------------------------
// Dates
// ---------------------------------------------------------------------------

// statedDate is one date found in a passage; Month and Day are zero when the
// passage does not state them.
type statedDate struct{ Year, Month, Day int }

const frenchMonthAlt = `(janvier|fevrier|mars|avril|mai|juin|juillet|aout|septembre|octobre|novembre|decembre|janv\.?|fevr\.?|fev\.?|avr\.?|juil\.?|sept\.?|oct\.?|nov\.?|dec\.?)`

var (
	reDayMonthName = regexp.MustCompile(`\b(1er|\d{1,2})\s+` + frenchMonthAlt + `\s+(\d{4})\b`)
	reMonthName    = regexp.MustCompile(`\b` + frenchMonthAlt + `\s+(\d{4})\b`)
	reNumericDMY   = regexp.MustCompile(`\b(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4}|\d{2})\b`)
	reISODate      = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})(?:[^\d]|$)`)
	reNumericMY    = regexp.MustCompile(`\b(\d{1,2})/(\d{4})\b`)
	// Ranges state their year once: "du 20 mars au 7 avril 2025", "du 20 au
	// 31 mars 2025", "les 19 et 20 mars 2025". The closing date is read by
	// reDayMonthName; these give the opening one its year.
	reRangeDayMonth = regexp.MustCompile(`\b(1er|\d{1,2})\s+` + frenchMonthAlt + `\s+(?:au|a|et|-)\s+(?:le\s+)?(?:1er|\d{1,2})\s+` + frenchMonthAlt + `\s+(\d{4})\b`)
	reRangeDay      = regexp.MustCompile(`\b(1er|\d{1,2})\s+(?:au|a|et|-)\s+(?:le\s+)?(?:1er|\d{1,2})\s+` + frenchMonthAlt + `\s+(\d{4})\b`)
	reYear          = regexp.MustCompile(`\b(1[89]\d{2}|2[01]\d{2})\b`)
)

// frenchMonthIndex maps a folded month name or abbreviation onto 1..12.
func frenchMonthIndex(name string) int {
	name = strings.TrimSuffix(name, ".")
	switch {
	case strings.HasPrefix(name, "janv"):
		return 1
	case strings.HasPrefix(name, "fev"):
		return 2
	case name == "mars":
		return 3
	case strings.HasPrefix(name, "avr"):
		return 4
	case name == "mai":
		return 5
	case name == "juin":
		return 6
	case strings.HasPrefix(name, "juil"):
		return 7
	case name == "aout":
		return 8
	case strings.HasPrefix(name, "sept"):
		return 9
	case strings.HasPrefix(name, "oct"):
		return 10
	case strings.HasPrefix(name, "nov"):
		return 11
	case strings.HasPrefix(name, "dec"):
		return 12
	}
	return 0
}

func validDay(y, m, d int) bool {
	if m < 1 || m > 12 || d < 1 || y < 1000 {
		return false
	}
	days := [...]int{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if d > days[m-1] {
		return false
	}
	if m == 2 && d == 29 {
		return y%4 == 0 && (y%100 != 0 || y%400 == 0)
	}
	return true
}

func expandYear(s string) int {
	y, _ := strconv.Atoi(s)
	if len(s) == 2 {
		if y < 70 {
			return 2000 + y
		}
		return 1900 + y
	}
	return y
}

// findStatedDates lists every date a passage states, in French usage: "3 mars
// 2025", "1er mars 2025", "03/03/2025", "2025-03-03", "mars 2025", "03/2025",
// "2025". Numeric dates are read day-first, as French documents write them.
func findStatedDates(text string) []statedDate {
	t := foldForMatch(text)
	var out []statedDate
	for _, m := range reDayMonthName.FindAllStringSubmatch(t, -1) {
		d := 1
		if m[1] != "1er" {
			d, _ = strconv.Atoi(m[1])
		}
		mo := frenchMonthIndex(m[2])
		y, _ := strconv.Atoi(m[3])
		if validDay(y, mo, d) {
			out = append(out, statedDate{y, mo, d})
		}
	}
	dayOf := func(s string) int {
		if s == "1er" {
			return 1
		}
		d, _ := strconv.Atoi(s)
		return d
	}
	for _, m := range reRangeDayMonth.FindAllStringSubmatch(t, -1) {
		d, mo, end := dayOf(m[1]), frenchMonthIndex(m[2]), frenchMonthIndex(m[3])
		y, _ := strconv.Atoi(m[4])
		if mo > end {
			y-- // "du 20 décembre au 5 janvier 2025"
		}
		if validDay(y, mo, d) {
			out = append(out, statedDate{y, mo, d})
		}
	}
	for _, m := range reRangeDay.FindAllStringSubmatch(t, -1) {
		d, mo := dayOf(m[1]), frenchMonthIndex(m[2])
		y, _ := strconv.Atoi(m[3])
		if validDay(y, mo, d) {
			out = append(out, statedDate{y, mo, d})
		}
	}
	for _, m := range reMonthName.FindAllStringSubmatch(t, -1) {
		if mo := frenchMonthIndex(m[1]); mo > 0 {
			y, _ := strconv.Atoi(m[2])
			out = append(out, statedDate{Year: y, Month: mo})
		}
	}
	for _, m := range reNumericDMY.FindAllStringSubmatch(t, -1) {
		d, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		y := expandYear(m[3])
		if validDay(y, mo, d) {
			out = append(out, statedDate{y, mo, d})
		}
	}
	for _, m := range reISODate.FindAllStringSubmatch(t, -1) {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if validDay(y, mo, d) {
			out = append(out, statedDate{y, mo, d})
		}
	}
	for _, m := range reNumericMY.FindAllStringSubmatch(t, -1) {
		mo, _ := strconv.Atoi(m[1])
		y, _ := strconv.Atoi(m[2])
		if mo >= 1 && mo <= 12 {
			out = append(out, statedDate{Year: y, Month: mo})
		}
	}
	for _, m := range reYear.FindAllStringSubmatch(t, -1) {
		y, _ := strconv.Atoi(m[1])
		out = append(out, statedDate{Year: y})
	}
	return out
}

// DateStatedIn reports whether one of the passages states d at a compatible
// precision: a day-precision claim needs that exact day, a month-precision
// claim that month (with or without a day), a year-precision claim that year.
// An unknown-precision date asserts nothing and always passes.
func DateStatedIn(d types.PartialDate, passages ...string) bool {
	if d.Precision == types.DatePrecisionUnknown || d.Precision == "" {
		return true
	}
	for _, p := range passages {
		for _, s := range findStatedDates(p) {
			if s.Year != d.Year {
				continue
			}
			switch d.Precision {
			case types.DatePrecisionYear:
				return true
			case types.DatePrecisionMonth:
				if s.Month == d.Month {
					return true
				}
			case types.DatePrecisionDay:
				if s.Month == d.Month && s.Day == d.Day {
					return true
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Amounts
// ---------------------------------------------------------------------------

// statedAmount is one amount found in a passage, with every qualifier stated
// right beside it.
type statedAmount struct {
	Minor      int64
	Currency   string
	Qualifiers map[string]bool
}

const amountNumber = `(\d{1,3}(?:[ .]\d{3})+|\d+)(?:[,.](\d{1,2}))?`

var (
	reAmountThenCurrency = regexp.MustCompile(`(?:^|[^\d.,])` + amountNumber + `\s*(€|euros?\b|eur\b|\$|usd\b|£|gbp\b|chf\b)`)
	reCurrencyThenAmount = regexp.MustCompile(`(€|\$|£|\beur\b|\busd\b|\bgbp\b|\bchf\b)\s*` + amountNumber + `\b`)
	reQualGross          = regexp.MustCompile(`\bbrut(e|s|es)?\b`)
	reQualNet            = regexp.MustCompile(`\bnet(te|s|tes)?\b`)
	reQualHT             = regexp.MustCompile(`\bht\b|hors tax`)
	reQualTTC            = regexp.MustCompile(`\bttc\b|toutes taxes comprises`)
)

func currencyCode(s string) string {
	switch strings.TrimSpace(s) {
	case "€", "eur", "euro", "euros":
		return "EUR"
	case "$", "usd":
		return "USD"
	case "£", "gbp":
		return "GBP"
	case "chf":
		return "CHF"
	}
	return ""
}

func parseAmount(intPart, decPart string) (int64, bool) {
	digits := strings.NewReplacer(" ", "", ".", "").Replace(intPart)
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || v > 1<<50 {
		return 0, false
	}
	cents := int64(0)
	switch len(decPart) {
	case 1:
		c, _ := strconv.Atoi(decPart)
		cents = int64(c) * 10
	case 2:
		c, _ := strconv.Atoi(decPart)
		cents = int64(c)
	}
	return v*100 + cents, true
}

// qualifiersNear reads the qualifiers stated right after an amount (up to the
// next figure, so "2 500 € brut, 1 950 € net" keeps them apart) and just
// before it.
func qualifiersNear(t string, start, end int) map[string]bool {
	after := t[end:]
	if len(after) > 40 {
		after = after[:40]
	}
	if i := strings.IndexFunc(after, unicode.IsDigit); i >= 0 {
		after = after[:i]
	}
	// A table row puts its label ("Total brut", "Net payé") far left of the
	// figure: the text layer's padding is not distance, so it is collapsed
	// before the label is read.
	before := strings.Join(strings.Fields(t[max(0, start-240):start]), " ")
	if len(before) > 16 {
		before = before[len(before)-16:]
	}
	if i := strings.LastIndexFunc(before, unicode.IsDigit); i >= 0 {
		before = before[i+1:]
	}
	window := before + " " + after
	q := map[string]bool{}
	if reQualGross.MatchString(window) {
		q[types.MoneyGross] = true
	}
	if reQualNet.MatchString(window) {
		q[types.MoneyNet] = true
	}
	if reQualHT.MatchString(window) {
		q[types.MoneyHT] = true
	}
	if reQualTTC.MatchString(window) {
		q[types.MoneyTTC] = true
	}
	return q
}

// findStatedAmounts lists every amount with a currency marker: "2 500,00 €",
// "1.234,56 euros", "€ 300", "12,50 EUR". A bare figure without currency is
// not an amount — it may be a count, a reference or a page number.
func findStatedAmounts(text string) []statedAmount {
	t := foldForMatch(text)
	var out []statedAmount
	for _, idx := range reAmountThenCurrency.FindAllStringSubmatchIndex(t, -1) {
		intPart := t[idx[2]:idx[3]]
		dec := ""
		if idx[4] >= 0 {
			dec = t[idx[4]:idx[5]]
		}
		if v, ok := parseAmount(intPart, dec); ok {
			out = append(out, statedAmount{v, currencyCode(t[idx[6]:idx[7]]), qualifiersNear(t, idx[2], idx[7])})
		}
	}
	for _, idx := range reCurrencyThenAmount.FindAllStringSubmatchIndex(t, -1) {
		intPart := t[idx[4]:idx[5]]
		dec := ""
		if idx[6] >= 0 {
			dec = t[idx[6]:idx[7]]
		}
		if v, ok := parseAmount(intPart, dec); ok {
			out = append(out, statedAmount{v, currencyCode(t[idx[2]:idx[3]]), qualifiersNear(t, idx[0], idx[1])})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Mentions printed in a written text
// ---------------------------------------------------------------------------

// TextMention is one date (day or month precision) or one amount printed in
// a text the agent wrote, at half-open code-point offsets. The verifier
// checks each against the sources of the claim that covers it, so a figure
// cannot reach the lawyer unchecked because the model left it out of the
// claim's structured fields.
type TextMention struct {
	Start, End int
	Date       *types.PartialDate
	Money      *types.Money
}

// FindTextMentions lists the dates and amounts printed in text. A year alone
// is not a mention (article and decision numbers are full of them), and a
// month inside a full date is the date, not a second mention.
func FindTextMentions(text string) []TextMention {
	t := foldForMatch(text)
	var out []TextMention
	add := func(bs, be int, d *types.PartialDate, m *types.Money) {
		out = append(out, TextMention{Start: runeOffset(t, bs), End: runeOffset(t, be), Date: d, Money: m})
	}
	day := func(y, mo, d int) *types.PartialDate {
		return &types.PartialDate{Precision: types.DatePrecisionDay, Year: y, Month: mo, Day: d}
	}
	for _, m := range reDayMonthName.FindAllStringSubmatchIndex(t, -1) {
		d := 1
		if t[m[2]:m[3]] != "1er" {
			d, _ = strconv.Atoi(t[m[2]:m[3]])
		}
		mo := frenchMonthIndex(t[m[4]:m[5]])
		y, _ := strconv.Atoi(t[m[6]:m[7]])
		if validDay(y, mo, d) {
			add(m[0], m[1], day(y, mo, d), nil)
		}
	}
	for _, m := range reNumericDMY.FindAllStringSubmatchIndex(t, -1) {
		d, _ := strconv.Atoi(t[m[2]:m[3]])
		mo, _ := strconv.Atoi(t[m[4]:m[5]])
		y := expandYear(t[m[6]:m[7]])
		if validDay(y, mo, d) {
			add(m[0], m[1], day(y, mo, d), nil)
		}
	}
	for _, m := range reISODate.FindAllStringSubmatchIndex(t, -1) {
		y, _ := strconv.Atoi(t[m[2]:m[3]])
		mo, _ := strconv.Atoi(t[m[4]:m[5]])
		d, _ := strconv.Atoi(t[m[6]:m[7]])
		if validDay(y, mo, d) {
			add(m[2], m[7], day(y, mo, d), nil)
		}
	}
	for _, m := range reMonthName.FindAllStringSubmatchIndex(t, -1) {
		if mo := frenchMonthIndex(t[m[2]:m[3]]); mo > 0 {
			y, _ := strconv.Atoi(t[m[4]:m[5]])
			add(m[0], m[1], &types.PartialDate{Precision: types.DatePrecisionMonth, Year: y, Month: mo}, nil)
		}
	}
	for _, m := range reNumericMY.FindAllStringSubmatchIndex(t, -1) {
		mo, _ := strconv.Atoi(t[m[2]:m[3]])
		y, _ := strconv.Atoi(t[m[4]:m[5]])
		if mo >= 1 && mo <= 12 && y >= 1000 {
			add(m[0], m[1], &types.PartialDate{Precision: types.DatePrecisionMonth, Year: y, Month: mo}, nil)
		}
	}
	money := func(intPart, dec, cur string, q map[string]bool) *types.Money {
		v, ok := parseAmount(intPart, dec)
		if !ok {
			return nil
		}
		m := &types.Money{MinorUnits: v, Currency: currencyCode(cur)}
		if len(q) == 1 {
			for k := range q {
				m.Qualifier = k
			}
		}
		return m
	}
	for _, idx := range reAmountThenCurrency.FindAllStringSubmatchIndex(t, -1) {
		dec := ""
		if idx[4] >= 0 {
			dec = t[idx[4]:idx[5]]
		}
		if m := money(t[idx[2]:idx[3]], dec, t[idx[6]:idx[7]], qualifiersNear(t, idx[2], idx[7])); m != nil {
			add(idx[2], idx[7], nil, m)
		}
	}
	for _, idx := range reCurrencyThenAmount.FindAllStringSubmatchIndex(t, -1) {
		dec := ""
		if idx[6] >= 0 {
			dec = t[idx[6]:idx[7]]
		}
		if m := money(t[idx[4]:idx[5]], dec, t[idx[2]:idx[3]], qualifiersNear(t, idx[0], idx[1])); m != nil {
			add(idx[0], idx[1], nil, m)
		}
	}
	// A mention inside a longer one ("mars 2025" in "3 mars 2025") is that
	// mention; overlapping matches of one figure count once.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End > out[j].End
	})
	kept := out[:0]
	for _, m := range out {
		if n := len(kept); n > 0 && m.Start < kept[n-1].End {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// MentionStatedIn reports whether one of the passages states the mention.
func MentionStatedIn(m TextMention, passages ...string) bool {
	if m.Date != nil {
		return DateStatedIn(*m.Date, passages...)
	}
	return m.Money != nil && AmountStatedIn(*m.Money, passages...)
}

// AmountStatedIn reports whether one of the passages states m: same value in
// minor units, same currency, and — when the claim carries a qualifier
// (brut, net, HT, TTC) — that qualifier stated beside it.
func AmountStatedIn(m types.Money, passages ...string) bool {
	for _, p := range passages {
		for _, s := range findStatedAmounts(p) {
			if s.Minor != m.MinorUnits || s.Currency != m.Currency {
				continue
			}
			if m.Qualifier == "" || s.Qualifiers[m.Qualifier] {
				return true
			}
		}
	}
	return false
}
