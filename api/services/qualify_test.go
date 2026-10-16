package services

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/Arkel-ai/nomos/api/config"
	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Qualification and classement, tested against a REAL Postgres and — for the
// qualification pass — the REAL gateway.
//
// The dossier below is a synthetic prud'hommes file: a CDI, an avenant, three
// bulletins de paie, two courriels (one of them undated on purpose), a mise en
// demeure, a convocation à entretien préalable, a certificat médical, a lettre
// de licenciement and an attestation de témoin. Nothing in it is a real
// person's document, and the filenames are deliberately uninformative
// ("scan_003.pdf") so the model has to read the text rather than the name.

const defaultTestDSN = "postgres://nomos:nomos@localhost:5433/nomos?sslmode=disable"

var openDBOnce sync.Once

// openTestDB connects, migrates and pins utils.DB. It FAILS rather than skips:
// the local dev Postgres is part of the toolchain, and a silently skipped
// suite is how a broken classement reaches a bordereau. Under
// NOMOS_TEST_NO_DOTENV=1 neither api/.env nor the dev DSN is consulted.
func openTestDB(t *testing.T) {
	t.Helper()
	openDBOnce.Do(func() {
		testmode.LoadDotenv("../.env")
		dsn := testmode.DatabaseURL(defaultTestDSN)
		if dsn == "" {
			t.Fatal("these tests need Postgres: DATABASE_URL is unset and NOMOS_TEST_NO_DOTENV=1 forbids the dev default")
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		})
		if err != nil {
			t.Fatalf("these tests need Postgres at %s: %v", dsn, err)
		}
		if err := db.AutoMigrate(types.NomosModels()...); err != nil {
			t.Fatalf("AutoMigrate: %v", err)
		}
		u.DB = db
	})
	if u.DB == nil {
		t.Fatal("no database handle")
	}
}

// pieceFixture is one synthetic document plus what a correct qualification
// must say about it.
type pieceFixture struct {
	name string
	text string
	// wantKind is the value from the closed vocabulary this document can only
	// reasonably be.
	wantKind string
	// wantDate is the ISO date the document bears, or "" when it bears none.
	wantDate string
}

var employmentFixtures = []pieceFixture{
	{
		name:     "scan_001.pdf",
		wantKind: PieceKindContratDeTravail,
		wantDate: "2019-09-12",
		text: `CONTRAT DE TRAVAIL À DURÉE INDÉTERMINÉE

ENTRE LES SOUSSIGNÉS :

La société DUPONT INDUSTRIE, SAS au capital de 250 000 euros, dont le siège
social est situé 14 rue des Lilas, 69003 Lyon, immatriculée au RCS de Lyon
sous le numéro 812 456 789, représentée par Monsieur Alain Dupont, agissant en
qualité de Président,
Ci-après dénommée « la Société »,
D'une part,

ET :

Monsieur Karim Benali, demeurant 8 avenue Jean Jaurès, 69007 Lyon,
Ci-après dénommé « le Salarié »,
D'autre part,

IL A ÉTÉ CONVENU CE QUI SUIT :

Article 1 — Engagement
La Société engage Monsieur Karim Benali en qualité de Technicien de
maintenance, statut employé, coefficient 250, à compter du 1er octobre 2019.

Article 2 — Durée du travail
La durée du travail est fixée à 35 heures hebdomadaires.

Article 3 — Rémunération
La rémunération brute mensuelle est fixée à 2 150 euros pour 151,67 heures.

Article 4 — Convention collective
Les parties déclarent que la convention collective applicable est celle de la
métallurgie.

Fait à Lyon, le 12 septembre 2019, en deux exemplaires originaux.

Pour la Société                          Le Salarié
Alain Dupont                             Karim Benali`,
	},
	{
		name:     "scan_002.pdf",
		wantKind: PieceKindAvenant,
		wantDate: "2021-03-01",
		text: `AVENANT N° 1 AU CONTRAT DE TRAVAIL

Entre la société DUPONT INDUSTRIE, SAS dont le siège social est 14 rue des
Lilas, 69003 Lyon, représentée par Monsieur Alain Dupont, Président,

Et Monsieur Karim Benali, Technicien de maintenance, engagé selon contrat à
durée indéterminée du 12 septembre 2019,

Il a été convenu ce qui suit :

Article 1
À compter du 1er avril 2021, Monsieur Karim Benali exercera les fonctions de
Chef d'équipe maintenance, statut agent de maîtrise, coefficient 305.

Article 2
La rémunération brute mensuelle est portée à 2 640 euros pour 151,67 heures.

Article 3
Les autres clauses du contrat de travail demeurent inchangées.

Fait à Lyon, le 1er mars 2021.

Pour la Société                          Le Salarié
Alain Dupont                             Karim Benali`,
	},
	{
		name:     "scan_003.pdf",
		wantKind: PieceKindBulletinDePaie,
		wantDate: "2022-01-31",
		text: `BULLETIN DE PAIE

Employeur : DUPONT INDUSTRIE SAS — 14 rue des Lilas, 69003 Lyon
SIRET 812 456 789 00021 — Code APE 2599B
Salarié : Monsieur Karim Benali — Chef d'équipe maintenance
Matricule 00147 — Emploi occupé depuis le 01/10/2019

Période du 01/01/2022 au 31/01/2022 — Paiement le 31 janvier 2022

Salaire de base            151,67 h      2 640,00
Prime d'ancienneté                          105,60
Heures supplémentaires 25 %     4,00 h     108,90

TOTAL BRUT                                2 854,50
Total des cotisations salariales            622,28
NET À PAYER                               2 044,12

Congés payés acquis : 12,50 jours`,
	},
	{
		name:     "scan_004.pdf",
		wantKind: PieceKindBulletinDePaie,
		wantDate: "2022-02-28",
		text: `BULLETIN DE PAIE

Employeur : DUPONT INDUSTRIE SAS — 14 rue des Lilas, 69003 Lyon
Salarié : Monsieur Karim Benali — Chef d'équipe maintenance
Matricule 00147

Période du 01/02/2022 au 28/02/2022 — Paiement le 28 février 2022

Salaire de base            151,67 h      2 640,00
Prime d'ancienneté                          105,60

TOTAL BRUT                                2 745,60
Total des cotisations salariales            598,54
NET À PAYER                               1 967,42`,
	},
	{
		name:     "scan_005.pdf",
		wantKind: PieceKindBulletinDePaie,
		wantDate: "2022-03-31",
		text: `BULLETIN DE PAIE

Employeur : DUPONT INDUSTRIE SAS — 14 rue des Lilas, 69003 Lyon
Salarié : Monsieur Karim Benali — Chef d'équipe maintenance
Matricule 00147

Période du 01/03/2022 au 31/03/2022 — Paiement le 31 mars 2022

Salaire de base            151,67 h      2 640,00
Prime d'ancienneté                          105,60
Absence non rémunérée        - 7,00 h    - 121,85

TOTAL BRUT                                2 623,75
Total des cotisations salariales            571,98
NET À PAYER                               1 879,63`,
	},
	{
		name:     "scan_006.pdf",
		wantKind: PieceKindCourriel,
		wantDate: "2022-04-12",
		text: `De : Karim Benali <k.benali@dupont-industrie.fr>
À : Sophie Lambert <s.lambert@dupont-industrie.fr>
Cc : Alain Dupont <a.dupont@dupont-industrie.fr>
Date : mardi 12 avril 2022 09:41
Objet : Heures supplémentaires de mars non réglées

Bonjour Madame Lambert,

Je reviens vers vous concernant mon bulletin de paie de mars. Les sept heures
supplémentaires effectuées les 14 et 15 mars à la demande de Monsieur Dupont
n'y figurent pas, et une absence non rémunérée de sept heures a été retenue
alors que j'étais présent sur le site de Vénissieux ces deux journées.

Je vous joins le relevé des badgeages. Pouvez-vous me confirmer la
régularisation sur la paie d'avril ?

Cordialement,
Karim Benali
Chef d'équipe maintenance`,
	},
	{
		name:     "scan_007.pdf",
		wantKind: PieceKindMiseEnDemeure,
		wantDate: "2022-04-20",
		text: `Karim Benali
8 avenue Jean Jaurès
69007 Lyon

DUPONT INDUSTRIE SAS
À l'attention de Monsieur le Président
14 rue des Lilas
69003 Lyon

Lettre recommandée avec accusé de réception n° 1A 234 567 890 1

Lyon, le 20 avril 2022

Objet : Mise en demeure de régulariser mes heures supplémentaires

Monsieur le Président,

Malgré mes relances des 12 et 19 avril 2022 restées sans réponse, les heures
supplémentaires que j'ai effectuées les 14 et 15 mars 2022 ne m'ont toujours
pas été réglées, et une retenue pour absence injustifiée a été opérée à tort
sur mon bulletin de paie du mois de mars 2022.

Par la présente, je vous mets en demeure de procéder à la régularisation de ma
rémunération dans un délai de quinze jours à compter de la réception de ce
courrier.

À défaut, je me verrai contraint de saisir le conseil de prud'hommes de Lyon.

Je vous prie d'agréer, Monsieur le Président, l'expression de mes salutations
distinguées.

Karim Benali`,
	},
	{
		name:     "scan_008.pdf",
		wantKind: PieceKindCourriel,
		wantDate: "",
		text: `De : Sophie Lambert <s.lambert@dupont-industrie.fr>
À : Karim Benali <k.benali@dupont-industrie.fr>
Objet : RE: Heures supplémentaires de mars non réglées

Bonjour,

Le service paie a examiné votre demande. Aucune heure supplémentaire n'a été
validée par votre responsable pour la période concernée, et le relevé de
badgeage que vous produisez n'est pas exploitable.

Nous ne donnerons donc pas suite à votre demande de régularisation.

Bien à vous,
Sophie Lambert
Responsable des ressources humaines`,
	},
	{
		name:     "scan_009.pdf",
		wantKind: PieceKindConvocationEP,
		wantDate: "2022-05-04",
		text: `DUPONT INDUSTRIE SAS
14 rue des Lilas
69003 Lyon

Monsieur Karim Benali
8 avenue Jean Jaurès
69007 Lyon

Lettre recommandée avec accusé de réception

Lyon, le 4 mai 2022

Objet : Convocation à un entretien préalable à un éventuel licenciement

Monsieur,

Nous sommes conduits à envisager à votre égard une mesure de licenciement.

Conformément aux dispositions de l'article L. 1232-2 du code du travail, nous
vous convoquons à un entretien préalable qui se tiendra le mardi 12 mai 2022 à
10 heures, dans les locaux de la société, 14 rue des Lilas à Lyon 3e, en
présence de Madame Sophie Lambert, Responsable des ressources humaines.

Nous vous rappelons que vous pouvez vous faire assister lors de cet entretien
par une personne de votre choix appartenant au personnel de l'entreprise.

Nous vous prions d'agréer, Monsieur, l'expression de nos salutations
distinguées.

Alain Dupont
Président`,
	},
	{
		name:     "scan_010.pdf",
		wantKind: PieceKindCertificatMedical,
		wantDate: "2022-05-10",
		text: `Docteur Hélène Vasseur
Médecin généraliste
22 cours Gambetta, 69007 Lyon
RPPS 10002345678

CERTIFICAT MÉDICAL

Je soussignée, Docteur Hélène Vasseur, certifie avoir examiné ce jour
Monsieur Karim Benali, né le 3 juin 1986.

Son état de santé nécessite un arrêt de travail de quatorze jours, du 10 mai
2022 au 23 mai 2022 inclus, pour syndrome anxio-dépressif réactionnel.

Certificat établi à la demande de l'intéressé et remis en main propre pour
faire valoir ce que de droit.

Fait à Lyon, le 10 mai 2022

Docteur Hélène Vasseur`,
	},
	{
		name:     "scan_011.pdf",
		wantKind: PieceKindLettreLicenciement,
		wantDate: "2022-05-16",
		text: `DUPONT INDUSTRIE SAS
14 rue des Lilas
69003 Lyon

Monsieur Karim Benali
8 avenue Jean Jaurès
69007 Lyon

Lettre recommandée avec accusé de réception

Lyon, le 16 mai 2022

Objet : Notification de licenciement

Monsieur,

À la suite de l'entretien préalable du 12 mai 2022, auquel vous vous êtes
présenté sans être assisté, nous vous notifions par la présente votre
licenciement pour cause réelle et sérieuse.

Les motifs en sont les suivants : insubordination répétée envers votre
hiérarchie et refus d'exécuter les tâches relevant de vos fonctions les 14 et
15 mars 2022.

Votre préavis d'une durée de deux mois débutera à la date de première
présentation de ce courrier. Vous serez dispensé de son exécution, celui-ci
vous étant néanmoins rémunéré.

Votre certificat de travail, votre attestation France Travail et votre solde
de tout compte seront tenus à votre disposition au service du personnel.

Nous vous prions d'agréer, Monsieur, l'expression de nos salutations
distinguées.

Alain Dupont
Président`,
	},
	{
		name:     "scan_012.pdf",
		wantKind: PieceKindAttestation,
		wantDate: "2022-06-20",
		text: `ATTESTATION DE TÉMOIN

Je soussigné Monsieur Thomas Grangier, né le 14 février 1990 à Saint-Étienne,
demeurant 5 rue Voltaire, 69100 Villeurbanne, exerçant la profession de
technicien de maintenance, atteste sur l'honneur de ce qui suit.

J'ai travaillé aux côtés de Monsieur Karim Benali sur le site de Vénissieux
les 14 et 15 mars 2022. Nous avons l'un et l'autre prolongé notre service au
delà de l'horaire habituel, à la demande expresse de Monsieur Alain Dupont,
pour achever le remplacement de la ligne de production n° 3.

Monsieur Benali était présent sur le site ces deux journées et a exécuté sans
réserve les tâches qui lui étaient demandées.

J'ai été informé que la présente attestation est destinée à être produite en
justice et que toute fausse attestation de ma part m'exposerait aux sanctions
pénales prévues à l'article 441-7 du code pénal.

Fait à Villeurbanne, le 20 juin 2022

Thomas Grangier`,
	},
}

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

type dossierFixture struct {
	tenantID  string
	dossierID string
	repo      *u.TenantRepo
	pieceIDs  []string
}

// seedDossier creates a tenant, a dossier and one piece per fixture. Pieces
// start in `pending` with no kind and no date, exactly as ingestion leaves
// them.
func seedDossier(t *testing.T) *dossierFixture {
	t.Helper()
	openTestDB(t)

	f := &dossierFixture{
		tenantID:  "test-firm-" + uuid.NewString()[:8],
		dossierID: uuid.NewString(),
	}
	f.repo = u.ForTenantID(context.Background(), f.tenantID)

	if err := f.repo.Create(&types.Dossier{
		ID:         f.dossierID,
		Title:      "Benali c/ Dupont Industrie",
		MatterType: "prud_hommes",
		State:      types.DossierStateActive,
		CreatedBy:  "test",
	}); err != nil {
		t.Fatalf("create dossier: %v", err)
	}

	for _, fx := range employmentFixtures {
		p := types.Piece{
			ID:                  uuid.NewString(),
			DossierID:           f.dossierID,
			SourceAssetID:       uuid.NewString(),
			OriginalName:        fx.name,
			ClassificationState: types.PieceStatePending,
			TextChars:           len(fx.text),
		}
		if err := f.repo.Create(&p); err != nil {
			t.Fatalf("create piece %s: %v", fx.name, err)
		}
		f.pieceIDs = append(f.pieceIDs, p.ID)
	}

	t.Cleanup(func() {
		db := u.DB
		db.Exec("DELETE FROM piece_parties WHERE tenant_id = ?", f.tenantID)
		db.Exec("DELETE FROM pieces WHERE tenant_id = ?", f.tenantID)
		db.Exec("DELETE FROM dossiers WHERE tenant_id = ?", f.tenantID)
	})
	return f
}

func (f *dossierFixture) pieces(t *testing.T) []types.Piece {
	t.Helper()
	var rows []types.Piece
	if err := f.repo.Pieces().Where("dossier_id = ?", f.dossierID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("load pieces: %v", err)
	}
	return rows
}

func isoToUnix(t *testing.T, iso string) *int64 {
	t.Helper()
	if iso == "" {
		return nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", iso, time.UTC)
	if err != nil {
		t.Fatalf("bad fixture date %q: %v", iso, err)
	}
	unix := parsed.Unix()
	return &unix
}

// ---------------------------------------------------------------------------
// Qualification — the live pass
// ---------------------------------------------------------------------------

// TestQualifyPiecesFromFixtures runs the real qualification step over the whole
// synthetic dossier and asserts the two things the classement depends on: a
// piece kind from the CLOSED vocabulary, and a date that parsed.
func TestQualifyPiecesFromFixtures(t *testing.T) {
	f := seedDossier(t)
	requireGateway(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	for i, fx := range employmentFixtures {
		out, err := QualifyPiece(ctx, f.repo, f.dossierID, QualifyPieceInput{
			PieceID: f.pieceIDs[i],
			Text:    fx.text,
		}, u.CallRef{TenantID: f.tenantID})
		if err != nil {
			t.Fatalf("%s: QualifyPiece: %v", fx.name, err)
		}

		if !IsPieceKind(out.PieceKind) {
			t.Errorf("%s: piece_kind %q is outside the closed vocabulary", fx.name, out.PieceKind)
		}
		if out.PieceKind != fx.wantKind {
			t.Errorf("%s: piece_kind = %q, want %q", fx.name, out.PieceKind, fx.wantKind)
		}

		want := isoToUnix(t, fx.wantDate)
		switch {
		case want == nil && out.DocumentDate != nil:
			t.Errorf("%s: dated %s, but the document bears no date",
				fx.name, time.Unix(*out.DocumentDate, 0).UTC().Format("2006-01-02"))
		case want == nil && out.DatePrecision != types.DatePrecisionUnknown:
			t.Errorf("%s: date_precision = %q, want %q", fx.name, out.DatePrecision, types.DatePrecisionUnknown)
		case want != nil && out.DocumentDate == nil:
			t.Errorf("%s: no document_date parsed, want %s", fx.name, fx.wantDate)
		case want != nil && *out.DocumentDate != *want:
			t.Errorf("%s: document_date = %s, want %s", fx.name,
				time.Unix(*out.DocumentDate, 0).UTC().Format("2006-01-02"), fx.wantDate)
		}

		t.Logf("%-14s → %-32s %-12s parties=%d confidence=%.2f review=%t",
			fx.name, out.PieceKind, FormatPieceDate(out.DocumentDate, out.DatePrecision),
			out.PartyCount, out.Confidence, out.NeedsReview)
	}

	// Every qualified piece carries the summary the planner reads in place of
	// the document, and it says what the document says — the mise en demeure
	// is about unpaid overtime, which is in its body, not in its kind.
	for _, p := range f.pieces(t) {
		if p.ExtractionVersion != QualifyExtractionVersion {
			t.Errorf("%s: extraction_version = %q, want %q", p.OriginalName, p.ExtractionVersion, QualifyExtractionVersion)
		}
		if p.Summary == "" {
			t.Errorf("%s: no summary was stored", p.OriginalName)
		}
		if p.OriginalName == "scan_007.pdf" && !strings.Contains(strings.ToLower(p.Summary), "heures supplémentaires") {
			t.Errorf("%s: the summary misses what the letter is about: %q", p.OriginalName, p.Summary)
		}
		t.Logf("%-14s résumé: %s", p.OriginalName, p.Summary)
	}

	// Parties are ROWS, not a JSON blob (§9.1(6)) — and every one carries a
	// normalized_name, which is what makes reconciliation across pieces
	// possible at all.
	var parties []types.PiecePartie
	if err := f.repo.PieceParties().Order("piece_id, role, display_name").Find(&parties).Error; err != nil {
		t.Fatalf("load parties: %v", err)
	}
	if len(parties) == 0 {
		t.Fatal("no piece_parties rows were written")
	}
	for _, p := range parties {
		if p.NormalizedName == "" {
			t.Errorf("party %q has no normalized_name", p.DisplayName)
		}
	}
	t.Logf("piece_parties: %d rows across %d pieces", len(parties), len(f.pieceIDs))

	// Idempotency: the runner is at-least-once, so a replayed attempt must
	// update the piece and REPLACE its parties, never append a second set.
	before := countParties(t, f, f.pieceIDs[0])
	if _, err := QualifyPiece(ctx, f.repo, f.dossierID, QualifyPieceInput{
		PieceID: f.pieceIDs[0],
		Text:    employmentFixtures[0].text,
	}, u.CallRef{TenantID: f.tenantID}); err != nil {
		t.Fatalf("re-qualify: %v", err)
	}
	if after := countParties(t, f, f.pieceIDs[0]); after != before {
		t.Errorf("re-running qualify_piece changed the party count: %d → %d", before, after)
	}
}

// requireGateway gates the paid qualification pass: excluded under
// NOMOS_LIVE=0, and a missing key fails it under NOMOS_REQUIRE_DEPENDENCIES=1.
func requireGateway(t *testing.T) {
	t.Helper()
	testmode.Live(t, "live qualification pass")
	testmode.LoadDotenv("../.env")
	if config.Load().OpenRouterAPIKey == "" {
		testmode.Missing(t, "OPENROUTER_API_KEY not set — skipping the live qualification pass")
	}
}

func countParties(t *testing.T, f *dossierFixture, pieceID string) int64 {
	t.Helper()
	var n int64
	if err := f.repo.PieceParties().Where("piece_id = ?", pieceID).Count(&n).Error; err != nil {
		t.Fatalf("count parties: %v", err)
	}
	return n
}

// TestQualifyDateParsing pins the rule that keeps a fabricated day out of a
// filed document: the VALUE's shape decides the precision, not the model's
// claim about it.
func TestQualifyDateParsing(t *testing.T) {
	cases := []struct {
		raw, declared string
		wantISO       string
		wantPrecision string
	}{
		{"2019-09-12", types.DatePrecisionDay, "2019-09-12", types.DatePrecisionDay},
		{"2020-03", types.DatePrecisionMonth, "2020-03-01", types.DatePrecisionMonth},
		{"2020-03", types.DatePrecisionDay, "2020-03-01", types.DatePrecisionMonth},
		{"2018", types.DatePrecisionYear, "2018-01-01", types.DatePrecisionYear},
		{"", types.DatePrecisionUnknown, "", types.DatePrecisionUnknown},
		{"le 12 septembre", types.DatePrecisionDay, "", types.DatePrecisionUnknown},
	}
	for _, c := range cases {
		got, precision := parseDocumentDate(c.raw, c.declared)
		if precision != c.wantPrecision {
			t.Errorf("%q: precision = %q, want %q", c.raw, precision, c.wantPrecision)
		}
		switch {
		case c.wantISO == "" && got != nil:
			t.Errorf("%q: expected no date, got %d", c.raw, *got)
		case c.wantISO != "" && got == nil:
			t.Errorf("%q: expected %s, got nothing", c.raw, c.wantISO)
		case c.wantISO != "" && time.Unix(*got, 0).UTC().Format("2006-01-02") != c.wantISO:
			t.Errorf("%q: got %s, want %s", c.raw, time.Unix(*got, 0).UTC().Format("2006-01-02"), c.wantISO)
		}
	}
}

// TestQualifyNormalizePartyName covers the reconciliation key: the same person
// written three ways must collapse to one key, or piece_parties stops being
// useful the moment a dossier has two documents.
func TestQualifyNormalizePartyName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Monsieur Karim Benali", "karim benali"},
		{"BENALI Karim", "benali karim"},
		{"M. Karim BENALI", "karim benali"},
		{"Société DUPONT INDUSTRIE (SAS)", "dupont industrie"},
		{"Docteur Hélène Vasseur", "helene vasseur"},
		{"Maître Étienne Lefèvre", "etienne lefevre"},
	}
	for _, c := range cases {
		if got := NormalizePartyName(c.in); got != c.want {
			t.Errorf("NormalizePartyName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Classement — deterministic, no gateway
// ---------------------------------------------------------------------------

// TestPieceKindVocabularyIsCovered guards the places the closed vocabulary
// is consumed. A kind with no label prints raw snake_case on a bordereau
// line — silent, and only visible in an export a court reads.
//
// The frontend holds another copy (front/src/lib/api/pieces.ts, and the
// `pieces.kinds` labels in front/locales/*.json). Go cannot assert it here, so
// UpdatePiece rejects an out-of-vocabulary correction with a 400 rather than
// storing it.
func TestPieceKindVocabularyIsCovered(t *testing.T) {
	for _, kind := range PieceKinds() {
		if !IsPieceKind(kind) {
			t.Errorf("%s: not recognised by IsPieceKind", kind)
		}
		if _, ok := pieceKindLabels[kind]; !ok {
			t.Errorf("%s: no French wording for the bordereau line", kind)
		}
	}
}
