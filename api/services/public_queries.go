package services

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

var publicQueryLog = u.NewLogger("services.public_queries")

// The public-query confidentiality gate — owner decision 2 (docs/PLAN.md),
// replacing the PublicQuerySpec catalogue.
//
// A query leaving the cabinet (web search, Légifrance, Judilibre) is checked
// against a DETERMINISTIC deny-list computed at query time from the tenant
// and the turn's dossier. A hit rejects the query with unsafe_public_query and
// the category, so the agent can reformulate in general legal terms; nothing
// is ever silently rewritten. Rejections are logged by category code only —
// never the query, never the matched text.
//
// This is a deny-list, not a proof of anonymity. It catches what can be
// recognised mechanically: the names the qualification extracted, the
// dossier's own title, contact and identity numbers, amounts, the exact dates
// the pièces carry, and any 6-word run copied from a pièce. A false positive
// costs a reformulation; a false negative leaks client facts, so every
// ambiguity resolves towards rejection.

// Rejection categories.
const (
	PublicQueryShape        = "query_shape" // empty, too long, multi-line
	PublicQueryPartyName    = "party_name"
	PublicQueryDossierTitle = "dossier_title"
	PublicQueryEmail        = "email"
	PublicQueryPhone        = "phone"
	PublicQueryAddress      = "postal_address"
	PublicQueryIBAN         = "iban"
	PublicQuerySIRET        = "siret"
	PublicQuerySIREN        = "siren"
	PublicQueryNIR          = "nir"
	PublicQueryAmount       = "amount"
	PublicQueryPieceDate    = "piece_date"
	PublicQueryPieceText    = "piece_text"
	// PublicQueryUncheckable: the dossier's text exceeds what the gate can
	// scan, so no query can be shown not to copy it.
	PublicQueryUncheckable = "dossier_uncheckable"
)

// pieceTextRunWords is owner decision 2's threshold: this many consecutive
// words found in a pièce make the query a copy of it.
const pieceTextRunWords = 6

// Deny-list input bounds. The party list is tenant-wide; the text scan reads
// the dossier's indexed chunks.
const (
	maxDenyPartyNames = 50000
	maxDenyTextBytes  = 32 << 20
)

// PublicQueryVerdict is the gate's answer. Matched holds the fragments OF THE
// QUERY that hit, so the agent knows what to remove; it never holds dossier
// text. It goes to the agent only, never to a log line.
type PublicQueryVerdict struct {
	Categories []string
	Matched    []string
}

// Safe reports whether the query may leave the cabinet.
func (v PublicQueryVerdict) Safe() bool { return len(v.Categories) == 0 }

func (v *PublicQueryVerdict) add(category, fragment string) {
	for _, c := range v.Categories {
		if c == category {
			return
		}
	}
	v.Categories = append(v.Categories, category)
	if fragment = strings.TrimSpace(fragment); fragment != "" {
		v.Matched = append(v.Matched, truncateRunes(fragment, 80))
	}
}

// webQueryIsSafe is the confidentiality gate, and it is a HARD one: it runs
// before the query leaves the process, on every query whatever wrote it.
//
// The :online backend is outside the provider allowlist and outside
// `data_collection: "deny"`, so this is the last place a piece of privileged
// material can be stopped. The rules are deliberately crude — a question of
// law is short and has no line breaks, dossier prose is long and has them.
func webQueryIsSafe(query string) error {
	// Trimmed HERE, not by the caller. This is the last check before a query
	// leaves the process, so it must hold whatever the caller did or forgot.
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("une recherche en ligne doit porter une question")
	}
	if n := len([]rune(query)); n > types.MaxWebQueryRunes {
		return fmt.Errorf("la requête fait %d caractères, maximum %d — une question de droit, pas un extrait de dossier", n, types.MaxWebQueryRunes)
	}
	if strings.ContainsAny(query, "\n\r") {
		return fmt.Errorf("la requête doit tenir en une seule ligne")
	}
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// CheckPublicQuery runs the whole gate for one query: shape, patterns, and
// the deny-list of the tenant and dossier (dossierID may be empty for a turn
// without one). lawyerText is what the lawyer typed in this thread and run —
// the instruction, the messages, the answers: the names and dates in it are
// denied too, which matters most for a turn without a dossier. An error
// means the deny-list could not be built — the caller must then refuse the
// query, never send it unchecked.
func CheckPublicQuery(ctx context.Context, repo *u.TenantRepo, dossierID, query string, lawyerText ...string) (PublicQueryVerdict, error) {
	var v PublicQueryVerdict
	if err := webQueryIsSafe(query); err != nil {
		v.add(PublicQueryShape, "")
		return v, nil
	}
	checkPublicPatterns(query, &v)
	deny, err := buildDenyList(ctx, repo, dossierID)
	if err != nil {
		return v, err
	}
	for _, t := range lawyerText {
		deny.addLawyerText(t)
	}
	deny.check(query, &v)
	if !v.Safe() {
		publicQueryLog.Infof("Public query rejected (tenant=%s categories=%s)", repo.TenantID(), strings.Join(v.Categories, ","))
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Patterns — no dossier needed
// ---------------------------------------------------------------------------

var (
	reEmail = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	// French numbers (01 23 45 67 89, +33 1 23…, 0033…) and other
	// international ones.
	rePhoneFR   = regexp.MustCompile(`(?:\+33[\s.\-]?|0033[\s.\-]?|\b0)[1-9](?:[\s.\-]?\d{2}){4}\b`)
	rePhoneIntl = regexp.MustCompile(`\+\d{2,3}[\s.\-]?\d{1,4}(?:[\s.\-]?\d{2,4}){2,5}`)
	reIBAN      = regexp.MustCompile(`(?i)\b[a-z]{2}\d{2}(?:[ ]?[a-z0-9]){11,30}\b`)
	// NIR: sex, year, month (or 20-99 for special cases), department
	// (incl. 2A/2B), commune, order, optional key.
	reNIR   = regexp.MustCompile(`(?i)\b[12][ ]?\d{2}[ ]?(?:0[1-9]|1[0-2]|[2-9]\d)[ ]?(?:\d{2}|2[ab])[ ]?\d{3}[ ]?\d{3}(?:[ ]?\d{2})?\b`)
	reSIRET = regexp.MustCompile(`\b\d{3}[ \x{00A0}]?\d{3}[ \x{00A0}]?\d{3}[ \x{00A0}]?\d{5}\b`)
	reSIREN = regexp.MustCompile(`\b\d{3}[ \x{00A0}]?\d{3}[ \x{00A0}]?\d{3}\b`)
	// A street address: a number then a street word; a postal box; a
	// postal code followed by a capitalised town.
	reStreet   = regexp.MustCompile(`(?i)\b\d{1,4}\s*(?:bis|ter)?\s*,?\s+(?:rue|avenue|av\.?|boulevard|bd|place|chemin|impasse|all[ée]e|quai|cours|route|square|passage|voie|faubourg|rond-point|lotissement|r[ée]sidence|cit[ée]|parvis|esplanade|sentier|hameau|lieu-dit)\b`)
	rePostBox  = regexp.MustCompile(`(?i)\b(?:cedex|bp\s?\d+|cs\s?\d{3,})\b`)
	rePostCode = regexp.MustCompile(`\b(?:0[1-9]|[1-8]\d|9[0-8])\d{3}\s+\p{Lu}[\p{L}\-']+`)
	// An amount with a currency, either side.
	reAmount = regexp.MustCompile(`(?i)(?:\d[\d\s\x{00A0}\x{202F}.,]*\d|\d)\s?(?:€|k€|m€|eur\b|euros?\b|usd\b|\$|£|chf\b)|(?:€|\$|£)\s?\d`)
)

// checkPublicPatterns applies the patterns that need no dossier. Identity
// numbers are tested longest first so a NIR is not reported as a SIRET.
func checkPublicPatterns(q string, v *PublicQueryVerdict) {
	if m := reEmail.FindString(q); m != "" {
		v.add(PublicQueryEmail, m)
	}
	if m := rePhoneFR.FindString(q); m != "" {
		v.add(PublicQueryPhone, m)
	} else if m := rePhoneIntl.FindString(q); m != "" {
		v.add(PublicQueryPhone, m)
	}
	if m := reIBAN.FindString(q); m != "" && hasDigits(m, 10) {
		v.add(PublicQueryIBAN, m)
	}
	rest := q
	if m := reNIR.FindString(rest); m != "" {
		v.add(PublicQueryNIR, m)
		rest = strings.Replace(rest, m, " ", 1)
	}
	if m := reSIRET.FindString(rest); m != "" {
		v.add(PublicQuerySIRET, m)
		rest = strings.Replace(rest, m, " ", 1)
	}
	if m := reSIREN.FindString(rest); m != "" {
		v.add(PublicQuerySIREN, m)
	}
	for _, re := range []*regexp.Regexp{reStreet, rePostBox, rePostCode} {
		if m := re.FindString(q); m != "" {
			v.add(PublicQueryAddress, m)
			break
		}
	}
	if m := reAmount.FindString(q); m != "" {
		v.add(PublicQueryAmount, m)
	}
}

func hasDigits(s string, n int) bool {
	c := 0
	for _, r := range s {
		if unicode.IsDigit(r) {
			c++
		}
	}
	return c >= n
}

// ---------------------------------------------------------------------------
// Dates
// ---------------------------------------------------------------------------

var frenchMonthNumbers = map[string]int{
	"janvier": 1, "fevrier": 2, "mars": 3, "avril": 4, "mai": 5, "juin": 6, "juillet": 7,
	"aout": 8, "septembre": 9, "octobre": 10, "novembre": 11, "decembre": 12,
}

var (
	reDateNumeric = regexp.MustCompile(`\b(\d{1,2})[/.\-](\d{1,2})[/.\-](\d{4})\b`)
	reDateISO     = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
	reDateLong    = regexp.MustCompile(`(?i)\b(\d{1,2})(?:er)?\s+(janvier|f[ée]vrier|mars|avril|mai|juin|juillet|ao[ûu]t|septembre|octobre|novembre|d[ée]cembre)\s+(\d{4})\b`)
	// A date introduced by a public enactment is the enactment's identity,
	// not a client fact: "loi du 8 août 2016", "décret n° 2008-244 du 7 mars
	// 2008". Court decisions and agreements are NOT exempt — the client's
	// own ordonnance or accord is exactly what must not leave.
	reEnactmentLead = regexp.MustCompile(`(?i)(?:\bloi|\bd[ée]cret|\bcirculaire|\bdirective|\bn[°o]\s*\d{2,4}-\d{1,5})[^.;:]{0,30}$`)
)

// dateMatch is one exact (day-precision) date found in a text.
type dateMatch struct {
	key   string // YYYY-MM-DD
	start int    // byte offset of the match in the text
	text  string
}

// exactDates finds day-precision dates. Month-only and year-only dates are
// not exact dates and are ignored.
func exactDates(s string) []dateMatch {
	var out []dateMatch
	add := func(y, m, d int, start int, text string) {
		if m < 1 || m > 12 || d < 1 || d > 31 || y < 1800 || y > 2200 {
			return
		}
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
		if t.Day() != d {
			return
		}
		out = append(out, dateMatch{key: t.Format("2006-01-02"), start: start, text: text})
	}
	for _, m := range reDateNumeric.FindAllStringSubmatchIndex(s, -1) {
		add(atoiSafe(s[m[6]:m[7]]), atoiSafe(s[m[4]:m[5]]), atoiSafe(s[m[2]:m[3]]), m[0], s[m[0]:m[1]])
	}
	for _, m := range reDateISO.FindAllStringSubmatchIndex(s, -1) {
		add(atoiSafe(s[m[2]:m[3]]), atoiSafe(s[m[4]:m[5]]), atoiSafe(s[m[6]:m[7]]), m[0], s[m[0]:m[1]])
	}
	for _, m := range reDateLong.FindAllStringSubmatchIndex(s, -1) {
		month := frenchMonthNumbers[foldAccents(strings.ToLower(s[m[4]:m[5]]))]
		add(atoiSafe(s[m[6]:m[7]]), month, atoiSafe(s[m[2]:m[3]]), m[0], s[m[0]:m[1]])
	}
	return out
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

// foldAccents lowercases nothing; it maps accented letters onto ASCII with
// the same table party-name normalisation uses.
func foldAccents(s string) string {
	var b strings.Builder
	for _, r := range s {
		if repl, ok := accentFolding[unicode.ToLower(r)]; ok {
			b.WriteString(repl)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// wordTokens lowercases, folds accents and splits on anything that is not a
// letter or a digit: "l'article L. 1234-5" → [l article l 1234 5].
func wordTokens(s string) []string {
	folded := foldAccents(strings.ToLower(s))
	return strings.FieldsFunc(folded, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// commonWords are the French words — everyday, legal and corporate — that a
// name or a dossier title may contain without identifying anyone. A token of
// a name or title is treated as identifying unless it is listed here, so the
// list errs on the side of rejection: an unlisted ordinary word costs a
// reformulation, never a leak.
var commonWords = func() map[string]bool {
	words := strings.Fields(`
	a au aux avec chez contre dans de des du en et la le les leur leurs par pour sans sous sur un une
	ou ni mais donc car que qui quoi dont ce cet cette ces son sa ses notre nos votre vos
	dossier dossiers affaire affaires client cliente clients test essai nouveau nouvelle projet note
	contentieux litige litiges proces procedure procedures instance appel cassation pourvoi refere
	conseil conseils prud prudhommes prudhomme hommes prud'hommes tribunal tribunaux cour cours chambre
	juridiction judiciaire administratif administrative commercial commerciale civil civile penal penale
	social sociale sociales travail emploi salarie salariee salaries employeur employeurs employe
	licenciement licenciements rupture conventionnelle demission faute grave lourde reelle serieuse
	cause motif motifs economique economiques personnel personnelle disciplinaire sanction sanctions
	harcelement moral morale sexuel discrimination egalite remuneration salaire salaires prime primes
	heures supplementaires temps partiel complet duree determinee indeterminee cdd cdi contrat contrats
	avenant clause clauses non concurrence mobilite periode essai preavis indemnite indemnites
	conge conges payes maladie accident inaptitude reclassement medecin inspection inspecteur
	retraite prevoyance mutuelle cotisations cotisation urssaf assurance assurances
	bail baux loyer loyers location commercial habitation vente achat acquisition cession
	societe societes entreprise entreprises groupe holding filiale association syndicat syndicats
	comite cse delegue delegues representant representants personnel direction directeur directrice
	gerant gerante president presidente administrateur liquidateur mandataire liquidation redressement
	sauvegarde judiciaire faillite creancier creanciers debiteur dette dettes creance creances
	transport transports logistique services service conseil industrie industries batiment
	construction immobilier immobiliere commerce distribution banque bancaire finance financiere
	assurance hopital clinique ecole universite mairie commune departement region etat
	france francais francaise international internationale europe europeenne nationale national
	nord sud est ouest centre grand grande petit petite haut haute bas basse saint sainte
	responsabilite prejudice dommages interets reparation garantie garanties obligation obligations
	execution inexecution resolution resiliation nullite prescription forclusion delai delais
	divorce separation garde pension alimentaire succession heritage donation testament
	propriete copropriete voisinage servitude permis urbanisme environnement
	marque brevet droit droits auteur concurrence deloyale consommation consommateur
	penal plainte victime infraction delit contravention crime garde vue
	rapport rapports expertise expert constat attestation attestations courrier courriers lettre
	mise demeure convocation entretien prealable notification jugement arret ordonnance decision
	requete assignation conclusions memoire bordereau pieces piece synthese chronologie
	premier premiere second seconde deuxieme troisieme dernier derniere autre autres
	general generale public publique prive privee
	sa sas sasu sarl eurl sci snc scp selarl selas cie ets
	cph tj tgi ca ce cjue cedh qpc ccn smic rtt ijss ani cfdt cgt cgc cftc unsa`)
	m := make(map[string]bool, len(words))
	for _, w := range words {
		for _, t := range wordTokens(w) {
			m[t] = true
		}
	}
	return m
}()

// publicInstitutions are parties whose name is public and generic: a query
// about the URSSAF or France Travail names no client.
var publicInstitutions = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range []string{
		"urssaf", "pole emploi", "france travail", "cpam", "caisse primaire d assurance maladie",
		"carsat", "msa", "ags", "unedic", "inspection du travail", "dreets", "direccte", "dgt",
		"cnil", "tresor public", "direction generale des finances publiques", "dgfip",
		"conseil de prud hommes", "cour d appel", "cour de cassation", "tribunal judiciaire",
		"tribunal de commerce", "tribunal administratif", "conseil d etat", "ministere du travail",
		"etat", "agent judiciaire de l etat", "defenseur des droits", "medecine du travail",
		"assurance maladie", "caf", "caisse d allocations familiales",
	} {
		m[strings.Join(wordTokens(n), " ")] = true
	}
	return m
}()

// publicInstitutionPrefix is how many leading tokens of a name form a public
// body's name — all of them for the body itself, fewer for « CAF Conseil
// Brindille », whose remaining words still name a client, 0 for none.
func publicInstitutionPrefix(tokens []string) int {
	best := 0
	for inst := range publicInstitutions {
		n := len(strings.Fields(inst))
		if n > best && n <= len(tokens) && strings.Join(tokens[:n], " ") == inst {
			best = n
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// The dossier deny-list
// ---------------------------------------------------------------------------

// publicDenyList is what the tenant and the dossier forbid. It is built per
// query from the current rows, so a party added a minute ago is covered.
type publicDenyList struct {
	// names are whole party names as token sequences ("jean luc moreau").
	names map[string]bool
	// nameTokens are the identifying single tokens of party names.
	nameTokens map[string]bool
	// titleTokens are the identifying tokens of the dossier's title and
	// client reference.
	titleTokens map[string]bool
	// dates are the exact dates the pièces carry (YYYY-MM-DD).
	dates map[string]bool
	// texts are the pièces' extracted texts, scanned for 6-word runs.
	texts []string
	// bounded is set when the texts exceed what one check may scan: the
	// unscanned part could be copied, so every query is refused.
	bounded bool
}

// buildDenyList loads the deny-list inputs: the tenant's party names, and
// for the dossier its title, its pièces' dates and their indexed text.
func buildDenyList(ctx context.Context, repo *u.TenantRepo, dossierID string) (*publicDenyList, error) {
	d := &publicDenyList{names: map[string]bool{}, nameTokens: map[string]bool{}, titleTokens: map[string]bool{}, dates: map[string]bool{}}

	var parties []string
	if err := repo.PieceParties().Distinct("display_name").Limit(maxDenyPartyNames).Pluck("display_name", &parties).Error; err != nil {
		return nil, err
	}
	for _, name := range parties {
		d.addName(name)
	}

	if dossierID == "" {
		return d, nil
	}
	var dossier types.Dossier
	if err := repo.Dossiers().Where("id = ?", dossierID).Take(&dossier).Error; err != nil {
		return nil, err
	}
	if !dossier.TitlePending {
		d.addTitle(dossier.Title)
	}
	d.addTitle(dossier.ClientReference)

	var pieces []types.Piece
	if err := repo.Pieces().Select("id", "document_date", "date_precision").
		Where("dossier_id = ?", dossierID).Find(&pieces).Error; err != nil {
		return nil, err
	}
	for _, p := range pieces {
		if p.DocumentDate != nil && p.DatePrecision == types.DatePrecisionDay {
			d.dates[time.Unix(*p.DocumentDate, 0).UTC().Format("2006-01-02")] = true
		}
	}

	var chunks []types.PieceChunk
	if err := repo.PieceChunks().Select("piece_id", "text").Where("dossier_id = ?", dossierID).
		Order("piece_id, ordinal").Find(&chunks).Error; err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(chunks))
	indexed := map[string]bool{}
	for _, c := range chunks {
		texts = append(texts, c.Text)
		indexed[c.PieceID] = true
	}
	// A piece whose text is extracted but not indexed yet (embedding still
	// running, or failed) is read from its text sidecar instead: it must not
	// be the hole a copied sentence slips through.
	for _, p := range pieces {
		if indexed[p.ID] {
			continue
		}
		text, found, err := blobText(ctx, textSidecarKey(repo.TenantID(), dossierID, p.ID))
		if err != nil {
			return nil, err
		}
		if found {
			texts = append(texts, text)
		}
	}
	size := 0
	for _, t := range texts {
		if size += len(t); size > maxDenyTextBytes {
			// Past the bound the scan would stall the call, and what is not
			// scanned could be copied: the gate refuses rather than guess.
			publicQueryLog.Warnf("Deny-list text scan bounded, queries refused (dossier=%s chunks=%d)", dossierID, len(texts))
			d.bounded = true
			break
		}
		d.texts = append(d.texts, t)
		for _, m := range exactDates(t) {
			d.dates[m.key] = true
		}
	}
	return d, nil
}

func (d *publicDenyList) addName(name string) {
	tokens := strings.Fields(NormalizePartyName(name))
	inst := publicInstitutionPrefix(tokens)
	if len(tokens) == 0 || inst == len(tokens) {
		return // a public body is public — by its whole name only
	}
	d.names[strings.Join(tokens, " ")] = true
	// Three letters is a surname too (« Roy », « Bal »): a short token
	// costs at worst a reformulation.
	for _, t := range tokens[inst:] {
		if identifyingToken(t, 3) {
			d.nameTokens[t] = true
		}
	}
}

// lawyerCivilities introduce a name in what the lawyer types.
var lawyerCivilities = map[string]bool{
	"m": true, "mr": true, "mme": true, "mlle": true, "monsieur": true, "madame": true, "mademoiselle": true,
	"me": true, "maitre": true, "dr": true, "docteur": true, "societe": true, "ste": true,
	"sa": true, "sas": true, "sasu": true, "sarl": true, "eurl": true, "sci": true,
}

// addLawyerText denies the names and exact dates the lawyer typed. A name is
// a word after a civility, a capitalised word inside a sentence, or a word
// in capitals; a capitalised first word of a sentence is ordinary French
// (« Contestation du licenciement… ») and is not taken, so the lawyer's own
// topic words stay searchable.
func (d *publicDenyList) addLawyerText(text string) {
	for _, m := range exactDates(text) {
		d.dates[m.key] = true
	}
	sentenceStart, prev := true, ""
	word := []rune{}
	flush := func() {
		if len(word) == 0 {
			return
		}
		w := string(word)
		word = word[:0]
		t := foldAccents(strings.ToLower(w))
		first, _ := utf8.DecodeRuneInString(w)
		caps := utf8.RuneCountInString(w) >= 3 && strings.ToUpper(w) == w && strings.ToLower(w) != w
		named := lawyerCivilities[prev] || caps || (unicode.IsUpper(first) && !sentenceStart)
		if named && identifyingToken(t, 3) {
			d.nameTokens[t] = true
		}
		prev, sentenceStart = t, false
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
			continue
		}
		flush()
		switch r {
		case '.', '!', '?', '\n', ':', ';', '«', '"', '(':
			// After a civility's abbreviation point (« M. Kowalczyk ») the
			// next word is the name, not a new sentence.
			sentenceStart = !lawyerCivilities[prev]
		}
	}
	flush()
}

// addTitle keeps the title's identifying words and its reference numbers
// (five digits or more: "RG 21/01234" yields 01234, never a bare "21").
func (d *publicDenyList) addTitle(title string) {
	for _, t := range wordTokens(title) {
		if identifyingToken(t, 3) || hasDigits(t, 5) {
			d.titleTokens[t] = true
		}
	}
}

// identifyingToken: long enough to mean something, not a common word, not a
// bare number.
func identifyingToken(t string, minRunes int) bool {
	if utf8.RuneCountInString(t) < minRunes || commonWords[t] {
		return false
	}
	for _, r := range t {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// check applies the dossier-derived categories to a query.
func (d *publicDenyList) check(q string, v *PublicQueryVerdict) {
	if d.bounded {
		v.add(PublicQueryUncheckable, "")
	}
	// Names: whole sequences, then identifying single tokens.
	nameTokens := strings.Fields(NormalizePartyName(q))
	for i := range nameTokens {
		for n := 1; n <= 8 && i+n <= len(nameTokens); n++ {
			seq := strings.Join(nameTokens[i:i+n], " ")
			if d.names[seq] && (n > 1 || identifyingToken(seq, 3)) {
				v.add(PublicQueryPartyName, seq)
			}
		}
		if d.nameTokens[nameTokens[i]] {
			v.add(PublicQueryPartyName, nameTokens[i])
		}
	}

	tokens := wordTokens(q)
	for _, t := range tokens {
		if d.titleTokens[t] {
			v.add(PublicQueryDossierTitle, t)
			break
		}
	}

	for _, m := range exactDates(q) {
		if !d.dates[m.key] {
			continue
		}
		if reEnactmentLead.MatchString(q[:m.start]) {
			continue
		}
		v.add(PublicQueryPieceDate, m.text)
		break
	}

	if run := d.copiedRun(tokens); run != "" {
		v.add(PublicQueryPieceText, run)
	}
}

// copiedRun returns the first run of pieceTextRunWords consecutive query
// words that also occurs consecutively in a pièce's text.
func (d *publicDenyList) copiedRun(tokens []string) string {
	if len(tokens) < pieceTextRunWords || len(d.texts) == 0 {
		return ""
	}
	grams := map[string]bool{}
	for i := 0; i+pieceTextRunWords <= len(tokens); i++ {
		grams[strings.Join(tokens[i:i+pieceTextRunWords], " ")] = true
	}
	for _, text := range d.texts {
		words := wordTokens(text)
		for i := 0; i+pieceTextRunWords <= len(words); i++ {
			g := strings.Join(words[i:i+pieceTextRunWords], " ")
			if grams[g] {
				return g
			}
		}
	}
	return ""
}

// sortedCategories is a stable rendering for observations and tests.
func sortedCategories(v PublicQueryVerdict) []string {
	out := append([]string(nil), v.Categories...)
	sort.Strings(out)
	return out
}
