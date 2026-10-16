package services

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Relevance and isolation tests for hybrid retrieval (docs/PLAN.md §6.1–§6.4).
//
// These are REAL end-to-end tests: a seeded French employment-law corpus, real
// bge-m3 embeddings, real Postgres, real HNSW and GIN indexes. A fusion tested
// against mocked scores tests the mock.
//
// What is asserted, and why each one matters:
//
//  1. RRF beats BOTH single arms across a query set that mixes the two ways a
//     lawyer searches — a natural-language question whose wording shares
//     almost nothing with the article (the lexical arm returns nothing at all,
//     because websearch_to_tsquery ANDs its terms), and a citation lookup
//     ("article L. 3121-27") that carries almost no semantic signal.
//  2. The as-of date returns the version in force AT THE DATE OF THE FACTS and
//     excludes the one that superseded it. This is the difference between
//     advising on the barème and advising on the law that applied in 2016.
//  3. A private search cannot reach another tenant's chunk, or another
//     dossier's, even with the real ids in hand.
//
// Requires the dev Postgres and a configured LLM gateway; skipped otherwise.
// The retrievalFixture cleans its own previous rows at setup rather than in a TestMain,
// so this file stays independent of anything else in the package.

const retrievalFixtureSourcePrefix = "test_ret_"

var (
	retrievalFixtureOnce sync.Once
	retrievalFixture     *retrievalCorpus
)

type retrievalCorpus struct {
	source   string // legal_documents.source — the isolation handle for every query
	tenantA  string
	tenantB  string
	dossierA string
	dossierB string
	// chunkByRef maps a retrievalFixture reference to the seeded legal_chunks.id.
	chunkByRef map[string]uint64
	// versionByRef maps a retrievalFixture reference to the seeded legal_versions.id.
	versionByRef map[string]string
	chunkCount   int
}

// seedLegalText is one seeded document + version + chunk.
type seedLegalText struct {
	ref              string
	label            string // citation label, also the head of the chunk text
	body             string
	kind             string
	jurisdiction     string
	formation        string
	publicationLevel string
	solution         string
	ecli             string
	pourvoi          string
	decisionDate     *int64
	validFrom        *int64
	validTo          *int64
	state            string
}

func (a seedLegalText) text() string { return a.label + " " + a.body }

func unixPtr(v int64) *int64 { return &v }

// Unix seconds for the dates the temporal test turns on.
const (
	ts20130701 = 1372636800 // 2013-07-01
	ts20170924 = 1506211200 // 2017-09-24 — the ordonnances take effect
	ts20160101 = 1451606400 // 2016-01-01 — the "date des faits" of the old dispute
	ts20201125 = 1606262400 // 2020-11-25
)

// targets are the articles the eval queries must find. They are ordinary rows
// in the corpus; nothing marks them as special at query time.
func retrievalTargetArticles() []seedLegalText {
	return []seedLegalText{
		{
			ref:   "fauteGrave",
			label: "Article L. 1234-5 du code du travail.",
			body: "La faute grave rend impossible le maintien du salarié dans l'entreprise pendant la durée du préavis. " +
				"Le salarié licencié pour faute grave est privé de l'indemnité compensatrice de préavis et de l'indemnité de licenciement, " +
				"et quitte l'entreprise dès la notification de la rupture.",
		},
		{
			ref:   "prescription",
			label: "Article L. 1471-1 du code du travail.",
			body: "Toute action portant sur l'exécution du contrat de travail se prescrit par deux ans à compter du jour où celui qui l'exerce " +
				"a connu ou aurait dû connaître les faits lui permettant d'exercer son droit. Toute action portant sur la rupture du contrat " +
				"de travail se prescrit par douze mois à compter de la notification de la rupture.",
		},
		{
			ref:   "harcelementMoral",
			label: "Article L. 1152-1 du code du travail.",
			body: "Aucun salarié ne doit subir les agissements répétés de harcèlement moral qui ont pour objet ou pour effet une dégradation " +
				"de ses conditions de travail susceptible de porter atteinte à ses droits et à sa dignité, d'altérer sa santé physique ou " +
				"mentale ou de compromettre son avenir professionnel.",
		},
		{
			ref:   "congesAcquisition",
			label: "Article L. 3141-3 du code du travail.",
			body: "Le salarié a droit à un congé de deux jours et demi ouvrables par mois de travail effectif chez le même employeur. " +
				"La durée totale du congé exigible ne peut excéder trente jours ouvrables. Le droit s'ouvre dès l'embauche.",
		},
		{
			ref:   "dureeLegale",
			label: "Article L. 3121-27 du code du travail.",
			body:  "La durée légale de travail effectif des salariés à temps complet est fixée à trente-cinq heures par semaine.",
		},
		{
			ref:   "ruptureConventionnelle",
			label: "Article L. 1237-13 du code du travail.",
			body: "La convention de rupture définit les conditions de celle-ci, notamment le montant de l'indemnité spécifique de rupture " +
				"conventionnelle. Elle fixe la date de rupture du contrat de travail, qui ne peut intervenir avant le lendemain du jour " +
				"de l'homologation. À compter de la date de sa signature par les deux parties, chacune d'entre elles dispose d'un délai " +
				"de quinze jours calendaires pour exercer son droit de rétractation.",
		},
		{
			ref:              "decisionPreuve",
			label:            "Cour de cassation, chambre sociale, 25 novembre 2020, pourvoi n° 17-19.523.",
			kind:             "decision",
			jurisdiction:     "Cour de cassation",
			formation:        "Chambre sociale",
			publicationLevel: "Publié au Bulletin",
			solution:         "Cassation partielle",
			ecli:             "ECLI:FR:CCAS:2020:SO01108",
			pourvoi:          "17-19.523",
			decisionDate:     unixPtr(ts20201125),
			body: "L'employeur ne peut mettre en oeuvre un dispositif de contrôle de l'activité des salariés qui n'a pas été porté " +
				"préalablement à leur connaissance. Le moyen de preuve tiré d'un tel dispositif est illicite ; toutefois, l'illicéité " +
				"n'entraîne pas nécessairement le rejet des débats, le juge devant apprécier si son utilisation porte atteinte au " +
				"caractère équitable de la procédure dans son ensemble.",
		},
	}
}

// retrievalTemporalArticles are the two versions of the SAME document: the wording in
// force before the ordonnances of 22 September 2017 and the barème that
// replaced it. Both are `active` — a repealed wording is still the applicable
// wording for facts that took place while it was in force (§6.4).
func retrievalTemporalArticles() []seedLegalText {
	return []seedLegalText{
		{
			ref:       "baremeOld",
			label:     "Article L. 1235-3 du code du travail, version en vigueur du 1er juillet 2013 au 23 septembre 2017.",
			validFrom: unixPtr(ts20130701),
			validTo:   unixPtr(ts20170924),
			body: "Si le licenciement d'un salarié survient pour une cause qui n'est pas réelle et sérieuse, le juge peut proposer la " +
				"réintégration du salarié dans l'entreprise, avec maintien de ses avantages acquis. Si l'une ou l'autre des parties refuse, " +
				"le juge octroie au salarié une indemnité à la charge de l'employeur qui ne peut être inférieure aux salaires des six " +
				"derniers mois.",
		},
		{
			ref:       "baremeNew",
			label:     "Article L. 1235-3 du code du travail, version en vigueur depuis le 24 septembre 2017.",
			validFrom: unixPtr(ts20170924),
			body: "Si le licenciement d'un salarié survient pour une cause qui n'est pas réelle et sérieuse, le juge octroie au salarié une " +
				"indemnité à la charge de l'employeur, dont le montant est compris entre les montants minimaux et maximaux fixés par le " +
				"barème exprimé en mois de salaire brut et fonction de l'ancienneté du salarié dans l'entreprise.",
		},
		{
			ref:       "abroge",
			label:     "Article L. 1235-14 du code du travail, abrogé.",
			state:     "abrogated",
			validFrom: unixPtr(ts20130701),
			body: "Les dispositions relatives à la sanction de l'irrégularité de la procédure de licenciement prévues au présent article " +
				"ne sont pas applicables aux entreprises employant habituellement moins de onze salariés.",
		},
	}
}

// retrievalDistractorThemes are the hard neighbours: same code, same vocabulary,
// adjacent article numbers. A corpus of unrelated text would make any
// retrieval strategy look good.
var retrievalDistractorThemes = []string{
	"Toute heure accomplie au-delà de la durée légale hebdomadaire est une heure supplémentaire qui ouvre droit à une majoration de salaire.",
	"Le contingent annuel d'heures supplémentaires est fixé par une convention ou un accord collectif d'entreprise ou d'établissement.",
	"La durée quotidienne de travail effectif par salarié ne peut excéder dix heures, sauf dérogation accordée dans des conditions déterminées par décret.",
	"Est considéré comme salarié à temps partiel le salarié dont la durée du travail est inférieure à la durée légale du travail.",
	"La convention individuelle de forfait en jours sur l'année ne peut être conclue qu'avec les cadres disposant d'une autonomie dans l'organisation de leur emploi du temps.",
	"Le contrat de travail à durée déterminée ne peut avoir ni pour objet ni pour effet de pourvoir durablement un emploi lié à l'activité normale et permanente de l'entreprise.",
	"La période d'essai permet à l'employeur d'évaluer les compétences du salarié dans son travail et au salarié d'apprécier si les fonctions occupées lui conviennent.",
	"L'employeur qui envisage de licencier un salarié le convoque à un entretien préalable par lettre recommandée ou remise en main propre contre décharge.",
	"La lettre de licenciement comporte l'énoncé du ou des motifs invoqués par l'employeur et fixe les limites du litige.",
	"Le salarié licencié alors qu'il compte huit mois d'ancienneté ininterrompus au service du même employeur a droit à une indemnité de licenciement.",
	"Aucun salarié ne peut être sanctionné ou licencié en raison de son origine, de son sexe, de ses moeurs ou de son état de santé.",
	"Le conseil de prud'hommes règle par voie de conciliation les différends qui peuvent s'élever à l'occasion de tout contrat de travail.",
	"L'employeur prend les mesures nécessaires pour assurer la sécurité et protéger la santé physique et mentale des travailleurs.",
	"Le comité social et économique est consulté sur les orientations stratégiques de l'entreprise et sur la politique sociale.",
	"Le salarié dont le contrat est suspendu pour maladie conserve le bénéfice des garanties de prévoyance dans les conditions prévues par l'accord.",
	"La convention collective applicable est celle dont relève l'activité principale exercée par l'employeur.",
	"Le règlement intérieur fixe les règles générales et permanentes relatives à la discipline ainsi que la nature et l'échelle des sanctions.",
	"L'employeur verse au salarié une indemnité compensatrice de congés payés lorsque le contrat est rompu avant que le salarié ait pu prendre ses congés.",
	"Le transfert d'une entité économique autonome conservant son identité entraîne le maintien des contrats de travail en cours.",
	"L'action en requalification d'un contrat à durée déterminée en contrat à durée indéterminée est portée directement devant le bureau de jugement.",
	"Le salarié protégé ne peut être licencié qu'après autorisation de l'inspecteur du travail.",
	"La mise à pied conservatoire prononcée dans l'attente de la décision définitive ne constitue pas une sanction disciplinaire.",
	"L'employeur ne peut sanctionner deux fois les mêmes faits fautifs.",
	"Le télétravail est mis en place dans le cadre d'un accord collectif ou, à défaut, d'une charte élaborée par l'employeur.",
	"Le solde de tout compte, établi par l'employeur, fait l'inventaire des sommes versées au salarié lors de la rupture du contrat.",
}

// retrievalDistractorCodes cycles through plausible article numbers so the corpus is
// numerically dense and a citation lookup has real competition.
var retrievalDistractorCodes = []string{"L. 3121-", "L. 1242-", "L. 1232-", "L. 1152-", "L. 3141-", "L. 1234-", "L. 2312-", "L. 4121-"}

const seedRetrievalCorpusSize = 200

func buildRetrievalCorpus() []seedLegalText {
	arts := append(retrievalTargetArticles(), retrievalTemporalArticles()...)
	i := 0
	for len(arts) < seedRetrievalCorpusSize {
		theme := retrievalDistractorThemes[i%len(retrievalDistractorThemes)]
		code := retrievalDistractorCodes[i%len(retrievalDistractorCodes)]
		num := 30 + i
		arts = append(arts, seedLegalText{
			ref:   fmt.Sprintf("filler_%d", i),
			label: fmt.Sprintf("Article %s%d du code du travail.", code, num),
			body:  theme,
		})
		i++
	}
	return arts
}

// setupRetrievalCorpus seeds the corpus once per test binary. Rows from a previous run
// are deleted first: the retrievalFixture is identified by its `source`, which is also
// what every query in this file restricts itself to, so a corpus loaded
// concurrently by anything else cannot perturb a measurement.
func setupRetrievalCorpus(t *testing.T) *retrievalCorpus {
	t.Helper()
	retrievalFixtureOnce.Do(func() {
		testmode.LoadDotenv("../.env")
		if dsn := testmode.DatabaseURL("postgres://nomos:nomos@localhost:5433/nomos?sslmode=disable"); dsn != "" {
			os.Setenv("DATABASE_URL", dsn)
		}
		if u.DB == nil {
			u.InitClients(config.Load())
		}
		if u.DB == nil || !u.LLMConfigured() {
			return
		}
		f, err := seedRetrievalCorpus(t)
		if err != nil {
			t.Fatalf("seed corpus: %v", err)
		}
		retrievalFixture = f
	})
	if retrievalFixture == nil {
		t.Skip("Postgres or the LLM gateway is not configured — retrieval suite skipped")
	}
	return retrievalFixture
}

func seedRetrievalCorpus(t *testing.T) (*retrievalCorpus, error) {
	t.Helper()
	ctx := context.Background()
	run := uuid.NewString()[:8]
	f := &retrievalCorpus{
		source:       retrievalFixtureSourcePrefix + run,
		tenantA:      "test_ret_firm_a_" + run,
		tenantB:      "test_ret_firm_b_" + run,
		dossierA:     "test_ret_dos_a_" + run,
		dossierB:     "test_ret_dos_b_" + run,
		chunkByRef:   map[string]uint64{},
		versionByRef: map[string]string{},
	}
	purgeStaleRetrievalCorpus(t)

	arts := buildRetrievalCorpus()
	texts := make([]string, len(arts))
	for i, a := range arts {
		texts[i] = a.text()
	}
	t.Logf("embedding %d seeded chunks with the configured embedding model…", len(texts))
	vectors, err := u.Embed(ctx, "", texts)
	if err != nil {
		return nil, fmt.Errorf("embed corpus: %w", err)
	}

	for i, a := range arts {
		docID := fmt.Sprintf("%s_doc_%03d", f.source, i)
		verID := fmt.Sprintf("%s_ver_%03d", f.source, i)
		kind := a.kind
		if kind == "" {
			kind = "article"
		}
		state := a.state
		if state == "" {
			state = legalVersionStateActive
		}
		jurisdiction := a.jurisdiction
		if jurisdiction == "" {
			jurisdiction = "France"
		}
		doc := types.LegalDocument{
			ID: docID, Source: f.source, SourceID: fmt.Sprintf("%03d", i), Kind: kind,
			Jurisdiction: jurisdiction, CitationLabel: a.label, LicenseCode: "LO-2.0",
			CanonicalURL: "https://www.legifrance.gouv.fr/" + docID,
		}
		if err := u.DB.WithContext(ctx).Create(&doc).Error; err != nil {
			return nil, err
		}
		ver := types.LegalVersion{
			ID: verID, LegalDocumentID: docID, SourceVersion: "1", Text: a.text(),
			ValidFrom: a.validFrom, ValidTo: a.validTo, DecisionDate: a.decisionDate,
			Formation: a.formation, PublicationLevel: a.publicationLevel, Solution: a.solution,
			ECLI: a.ecli, State: state,
		}
		if a.pourvoi != "" {
			ver.SourceMeta = types.JSONB(fmt.Sprintf(`{"pourvoi":%q}`, a.pourvoi))
		}
		if err := u.DB.WithContext(ctx).Create(&ver).Error; err != nil {
			return nil, err
		}
		emb := pgvector.NewHalfVector(vectors[i])
		chunk := types.LegalChunk{
			LegalVersionID: verID, Ordinal: 0, Text: a.text(),
			Embedding: &emb, EmbeddingModel: config.Load().EmbeddingModel,
		}
		if err := u.DB.WithContext(ctx).Create(&chunk).Error; err != nil {
			return nil, err
		}
		f.chunkByRef[a.ref] = chunk.ID
		f.versionByRef[a.ref] = verID
	}
	f.chunkCount = len(arts)

	// search_vector is written by application DML — not a generated column and
	// not a trigger, neither of which the no-DDL rule can create (§6.2). The
	// expression here is the one the loader must use.
	if err := u.DB.WithContext(ctx).Exec(`
		UPDATE legal_chunks c
		   SET search_vector = to_tsvector('french', unaccent(c.text))
		  FROM legal_versions v
		  JOIN legal_documents d ON d.id = v.legal_document_id
		 WHERE v.id = c.legal_version_id AND d.source = ?`, f.source).Error; err != nil {
		return nil, err
	}

	if err := seedPrivateChunks(ctx, f); err != nil {
		return nil, err
	}
	t.Logf("seeded %d public chunks (source=%s) and 4 private chunks", f.chunkCount, f.source)
	return f, nil
}

// retrievalPrivateTexts are two dossiers' worth of pieces. The two tenants hold the
// SAME sentence, which is what makes the isolation assertion meaningful: only
// the tenant predicate can tell them apart.
var retrievalPrivateTexts = []string{
	"Le salarié a été convoqué à un entretien préalable le 3 mars 2024 puis licencié pour faute grave le 18 mars 2024.",
	"Attestation de M. Durand, témoin des faits reprochés au salarié lors de la réunion du 12 février 2024.",
}

func seedPrivateChunks(ctx context.Context, f *retrievalCorpus) error {
	vecs, err := u.Embed(ctx, "", retrievalPrivateTexts)
	if err != nil {
		return fmt.Errorf("embed private chunks: %w", err)
	}
	for _, tenant := range []struct{ id, dossier string }{{f.tenantA, f.dossierA}, {f.tenantB, f.dossierB}} {
		repo := u.ForTenantID(ctx, tenant.id)
		for i, text := range retrievalPrivateTexts {
			emb := pgvector.NewHalfVector(vecs[i])
			chunk := types.PieceChunk{
				DossierID: tenant.dossier,
				PieceID:   fmt.Sprintf("%s_piece_%d", tenant.dossier, i),
				Ordinal:   0, PageFrom: 1, PageTo: 1,
				Text: text, Embedding: &emb, EmbeddingModel: config.Load().EmbeddingModel,
			}
			// Create stamps tenant_id from the repository, never from the
			// caller's struct — a row written under the wrong tenant is the
			// same breach as a read without the predicate.
			if err := repo.Create(&chunk); err != nil {
				return err
			}
			if err := repo.PieceChunks().Where("id = ?", chunk.ID).
				UpdateColumn("search_vector", gorm.Expr("to_tsvector('french', unaccent(?))", text)).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// purgeStaleRetrievalCorpus removes anything a previous run of this file left behind.
func purgeStaleRetrievalCorpus(t *testing.T) {
	t.Helper()
	like := retrievalFixtureSourcePrefix + "%"
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM legal_chunks WHERE legal_version_id IN (
			SELECT v.id FROM legal_versions v JOIN legal_documents d ON d.id = v.legal_document_id WHERE d.source LIKE ?)`, []any{like}},
		{`DELETE FROM legal_versions WHERE legal_document_id IN (SELECT id FROM legal_documents WHERE source LIKE ?)`, []any{like}},
		{`DELETE FROM legal_documents WHERE source LIKE ?`, []any{like}},
		{`DELETE FROM piece_chunks WHERE tenant_id LIKE ?`, []any{"test_ret_firm_%"}},
	}
	for _, s := range stmts {
		if err := u.DB.Exec(s.sql, s.args...).Error; err != nil {
			t.Fatalf("purge stale retrievalFixture rows: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// 1. RRF vs. its own arms
// ---------------------------------------------------------------------------

type retrievalEvalQuery struct {
	name    string
	flavour string // semantic = lexically awkward | lexical = citation or exact phrase
	query   string
	target  string
}

// retrievalEvalQueries are split deliberately down the middle. The semantic half is
// how a client describes a problem; the lexical half is how a lawyer looks up
// a reference. A retrieval layer that only serves one of them is half a
// product.
var retrievalEvalQueries = []retrievalEvalQuery{
	{"faute grave", "semantic", "mon patron peut-il me mettre dehors du jour au lendemain si j'ai fait une grosse bêtise ?", "fauteGrave"},
	{"prescription", "semantic", "combien de temps me reste-t-il pour attaquer mon employeur après avoir perdu mon emploi ?", "prescription"},
	{"harcèlement", "semantic", "on m'a mis à l'écart, humilié et rabaissé pendant des mois par ma hiérarchie", "harcelementMoral"},
	{"congés", "semantic", "je viens tout juste d'être embauché, ai-je déjà le droit de partir en vacances ?", "congesAcquisition"},
	{"citation durée", "lexical", "article L. 3121-27", "dureeLegale"},
	{"citation pourvoi", "lexical", "pourvoi n° 17-19.523", "decisionPreuve"},
	{"rétractation", "lexical", "rupture conventionnelle délai de rétractation quinze jours homologation", "ruptureConventionnelle"},
	{"barème", "lexical", "barème indemnité licenciement sans cause réelle et sérieuse", "baremeNew"},
}

func TestRetrievalHybridBeatsSingleArms(t *testing.T) {
	f := setupRetrievalCorpus(t)
	ctx := context.Background()

	ranks := map[string][]int{} // strategy → per-query rank (0 = miss)
	mrr := map[string]float64{} //
	for _, strategy := range Strategies {
		for _, q := range retrievalEvalQueries {
			opts := DefaultRetrievalOptions()
			opts.Strategy = strategy
			opts.Sources = []string{f.source}
			res, err := Retrieve(ctx, RetrievalRequest{Query: q.query, Corpus: types.CorpusPublic, Options: opts})
			if err != nil {
				t.Fatalf("%s / %s: %v", strategy, q.name, err)
			}
			if strategy == StrategyRRFRerank && !res.Reranked {
				t.Fatalf("rrf_rerank did not actually rerank — the gateway rejected the call")
			}
			rank := retrievalRankOf(res.Hits, f.chunkByRef[q.target])
			ranks[strategy] = append(ranks[strategy], rank)
			if rank > 0 {
				mrr[strategy] += 1 / float64(rank)
			}
		}
		mrr[strategy] /= float64(len(retrievalEvalQueries))
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "\nrank of the expected article (0 = not in the top %d), %d-chunk corpus\n\n", defaultTopK, f.chunkCount)
	fmt.Fprint(w, "query\tflavour")
	for _, s := range Strategies {
		fmt.Fprintf(w, "\t%s", s)
	}
	fmt.Fprintln(w)
	for i, q := range retrievalEvalQueries {
		fmt.Fprintf(w, "%s\t%s", q.name, q.flavour)
		for _, s := range Strategies {
			fmt.Fprintf(w, "\t%s", retrievalRankLabel(ranks[s][i]))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprint(w, "MRR\t")
	for _, s := range Strategies {
		fmt.Fprintf(w, "\t%.3f", mrr[s])
	}
	fmt.Fprint(w, "\n\n")
	w.Flush()

	// This test used to assert that bare RRF beats BOTH single arms. That
	// assertion was a belief, and the relevance harness measured it false: over
	// 88 graded French queries against the committed fixture corpus, the dense
	// arm scores nDCG@10 0.835, the lexical arm 0.380, and their fusion 0.761.
	// On a corpus this small, with a strong multilingual embedder, fusing a
	// weaker arm into a stronger one costs ranking quality — and the assertion
	// only ever passed because the lexical arm was near-empty (it ANDed every
	// query term), so "rrf" was really "vector plus noise".
	//
	// What the product actually ships is rrf_rerank, and THAT is what must not
	// regress: the reranker is what repairs the fusion. So the assertion now
	// guards the shipped configuration against the best single arm, which is
	// the property a user experiences. Do not "restore" the old comparison
	// without a measurement from api/eval that supports it.
	best := mrr[StrategyVector]
	bestName := StrategyVector
	if mrr[StrategyFTS] > best {
		best, bestName = mrr[StrategyFTS], StrategyFTS
	}
	if mrr[StrategyRRFRerank] < best {
		t.Errorf("the shipped strategy %s (%.3f) scored below the best single arm %s (%.3f)",
			StrategyRRFRerank, mrr[StrategyRRFRerank], bestName, best)
	}
	t.Logf("MRR by strategy: vector=%.3f fts=%.3f rrf=%.3f rrf_rerank=%.3f (shipped)",
		mrr[StrategyVector], mrr[StrategyFTS], mrr[StrategyRRF], mrr[StrategyRRFRerank])

	// Complementarity, not just an average: each arm must be the only one that
	// works on at least one query. That is what fusion is buying.
	vectorSaves, ftsSaves := 0, 0
	for i := range retrievalEvalQueries {
		v, l := ranks[StrategyVector][i], ranks[StrategyFTS][i]
		if v > 0 && l == 0 {
			vectorSaves++
		}
		if l > 0 && (v == 0 || v > l+2) {
			ftsSaves++
		}
	}
	if vectorSaves == 0 {
		t.Error("no query where the vector arm rescued a lexical miss — the query set proves nothing")
	}
	if ftsSaves == 0 {
		t.Error("no query where the lexical arm rescued a semantic miss — the query set proves nothing")
	}
	t.Logf("vector-only rescues=%d, lexical-only rescues=%d", vectorSaves, ftsSaves)
}

func retrievalRankOf(hits []Hit, chunkID uint64) int {
	for i, h := range hits {
		if h.ChunkID == chunkID {
			return i + 1
		}
	}
	return 0
}

func retrievalRankLabel(rank int) string {
	if rank == 0 {
		return "—"
	}
	return fmt.Sprintf("%d", rank)
}

// ---------------------------------------------------------------------------
// 2. Temporal filtering
// ---------------------------------------------------------------------------

// TestRetrievalAsOfDateExcludesSupersededVersion is the §6.4 rule: the
// applicable text is the one in force at the date of the FACTS. Retrieving
// today's wording for a 2016 dismissal is not a ranking defect, it is wrong
// advice.
func TestRetrievalAsOfDateExcludesSupersededVersion(t *testing.T) {
	f := setupRetrievalCorpus(t)
	ctx := context.Background()
	const query = "indemnité pour licenciement sans cause réelle et sérieuse"

	base := func() RetrievalOptions {
		o := DefaultRetrievalOptions()
		o.Strategy = StrategyRRF // deterministic: no model in the loop
		o.Sources = []string{f.source}
		o.TopK = 20
		return o
	}

	oldVer, newVer := f.versionByRef["baremeOld"], f.versionByRef["baremeNew"]

	// Facts of 2016: the pre-ordonnances wording, and only it.
	opts := base()
	opts.AsOfDate = ts20160101
	res, err := Retrieve(ctx, RetrievalRequest{Query: query, Corpus: types.CorpusPublic, Options: opts})
	if err != nil {
		t.Fatalf("as-of 2016: %v", err)
	}
	if !retrievalHasVersion(res.Hits, oldVer) {
		t.Errorf("as-of 2016-01-01 did not return the version in force at that date (%s)", oldVer)
	}
	if retrievalHasVersion(res.Hits, newVer) {
		t.Errorf("as-of 2016-01-01 returned the version that only took effect on 2017-09-24 (%s)", newVer)
	}

	// Today: the barème, and the wording it replaced is gone.
	opts = base()
	res, err = Retrieve(ctx, RetrievalRequest{Query: query, Corpus: types.CorpusPublic, Options: opts})
	if err != nil {
		t.Fatalf("as-of today: %v", err)
	}
	if !retrievalHasVersion(res.Hits, newVer) {
		t.Errorf("as-of today did not return the version in force today (%s)", newVer)
	}
	if retrievalHasVersion(res.Hits, oldVer) {
		t.Errorf("as-of today returned a version whose validity ended on 2017-09-24 (%s)", oldVer)
	}

	// The state filter is the second half of the rule: an abrogated article is
	// out of reach unless it is asked for by name.
	abrogated := f.versionByRef["abroge"]
	opts = base()
	res, err = Retrieve(ctx, RetrievalRequest{
		Query:   "irrégularité de la procédure de licenciement entreprises de moins de onze salariés",
		Corpus:  types.CorpusPublic,
		Options: opts,
	})
	if err != nil {
		t.Fatalf("default state filter: %v", err)
	}
	if retrievalHasVersion(res.Hits, abrogated) {
		t.Errorf("an abrogated version was returned by the default (in-force) filter")
	}
	opts = base()
	opts.States = []string{legalVersionStateActive, "abrogated"}
	res, err = Retrieve(ctx, RetrievalRequest{
		Query:   "irrégularité de la procédure de licenciement entreprises de moins de onze salariés",
		Corpus:  types.CorpusPublic,
		Options: opts,
	})
	if err != nil {
		t.Fatalf("widened state filter: %v", err)
	}
	if !retrievalHasVersion(res.Hits, abrogated) {
		t.Errorf("widening the state filter did not reach the abrogated version — the knob does nothing")
	}

	// Authority metadata must ride along on every hit, or a lawyer cannot
	// weigh what came back (§6.3).
	opts = base()
	res, err = Retrieve(ctx, RetrievalRequest{Query: "preuve illicite dispositif de contrôle des salariés", Corpus: types.CorpusPublic, Options: opts})
	if err != nil {
		t.Fatalf("decision metadata: %v", err)
	}
	var decision *Hit
	for i := range res.Hits {
		if res.Hits[i].ChunkID == f.chunkByRef["decisionPreuve"] {
			decision = &res.Hits[i]
		}
	}
	if decision == nil {
		t.Fatal("the seeded decision was not retrieved at all")
	}
	for label, got := range map[string]string{
		"jurisdiction":      decision.Jurisdiction,
		"formation":         decision.Formation,
		"publication level": decision.PublicationLevel,
		"solution":          decision.Solution,
		"ECLI":              decision.ECLI,
		"pourvoi":           decision.Pourvoi,
		"legal version id":  decision.LegalVersionID,
		"licence":           decision.LicenseCode,
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("hit is missing its %s — a lawyer cannot judge authority from it", label)
		}
	}
	if decision.DecisionDate == nil || *decision.DecisionDate != ts20201125 {
		t.Errorf("hit is missing its decision date")
	}
}

func retrievalHasVersion(hits []Hit, versionID string) bool {
	for _, h := range hits {
		if h.LegalVersionID == versionID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 3. Private retrieval isolation
// ---------------------------------------------------------------------------

// TestRetrievalPrivateIsTenantScoped plays the attacker: firm B searches with
// firm A's real dossier id, and with its own. Nothing of firm A's may come
// back, ever. In a product holding privileged dossiers this is a breach of
// secret professionnel, not a ranking bug (§6.1, §13).
func TestRetrievalPrivateIsTenantScoped(t *testing.T) {
	f := setupRetrievalCorpus(t)
	ctx := context.Background()
	const query = "entretien préalable licenciement pour faute grave"

	opts := DefaultRetrievalOptions()
	opts.Strategy = StrategyRRF
	repoB := u.ForTenantID(ctx, f.tenantB)

	// B searching A's dossier: the tenant predicate wins before the dossier
	// filter is even interesting.
	res, err := Retrieve(ctx, RetrievalRequest{
		Query: query, Corpus: types.CorpusPrivate, Repo: repoB, DossierID: f.dossierA, Options: opts,
	})
	if err != nil {
		t.Fatalf("cross-tenant search: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("firm B retrieved %d chunks from firm A's dossier — cross-tenant leak", len(res.Hits))
	}

	// B searching its own dossier: hits, and every one of them is B's.
	res, err = Retrieve(ctx, RetrievalRequest{
		Query: query, Corpus: types.CorpusPrivate, Repo: repoB, DossierID: f.dossierB, Options: opts,
	})
	if err != nil {
		t.Fatalf("own-tenant search: %v", err)
	}
	if len(res.Hits) == 0 {
		t.Fatal("firm B retrieved nothing from its own dossier")
	}
	var owners []string
	if err := repoB.PieceChunks().Where("dossier_id = ?", f.dossierB).Distinct().Pluck("tenant_id", &owners).Error; err != nil {
		t.Fatalf("owner check: %v", err)
	}
	if len(owners) != 1 || owners[0] != f.tenantB {
		t.Fatalf("private hits are owned by %v, expected only %s", owners, f.tenantB)
	}
	for _, h := range res.Hits {
		if h.Corpus != types.CorpusPrivate || h.PieceID == "" {
			t.Errorf("private hit is missing its piece provenance: %+v", h)
		}
		if !strings.HasPrefix(h.PieceID, f.dossierB) {
			t.Errorf("private hit %s does not belong to the searched dossier", h.PieceID)
		}
	}

	// A private search with no dossier is a bug, not a broad search.
	if _, err := Retrieve(ctx, RetrievalRequest{
		Query: query, Corpus: types.CorpusPrivate, Repo: repoB, Options: opts,
	}); err == nil {
		t.Error("a private search without a dossier scope was accepted")
	}
}
