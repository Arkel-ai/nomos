package utils

import (
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/Arkel-ai/nomos/api/types"
)

var dbLog = NewLogger("utils.db")

// DB is the package-level GORM handle. It is nil when DATABASE_URL is not
// configured; services MUST guard with a nil check and return 503 — the
// same "optional integration" contract as OpenAI / Costs.
var DB *gorm.DB

// BetterAuthUser maps the `user` table that Better Auth owns and
// migrates (via `npx @better-auth/cli migrate`). We only read it. Column
// names are camelCase because Better Auth quotes identifiers in its
// Postgres migrations, so we pin them explicitly.
type BetterAuthUser struct {
	ID            string `gorm:"column:id;primaryKey"`
	Name          string `gorm:"column:name"`
	Email         string `gorm:"column:email"`
	EmailVerified bool   `gorm:"column:emailVerified"`
}

func (BetterAuthUser) TableName() string { return "user" }

// BetterAuthSession maps the `session` table Better Auth writes on login.
// The Go backend validates a request by looking the session token up
// here (see GetUserFromSession) — Better Auth is the source of truth,
// the API never signs or verifies a JWT itself.
type BetterAuthSession struct {
	ID        string    `gorm:"column:id;primaryKey"`
	Token     string    `gorm:"column:token;uniqueIndex"`
	UserID    string    `gorm:"column:userId"`
	ExpiresAt time.Time `gorm:"column:expiresAt"`
}

func (BetterAuthSession) TableName() string { return "session" }

// initDB opens the GORM Postgres connection and auto-migrates the
// app-owned tables. Better Auth tables are NOT migrated here — Better
// Auth owns them. Casbin's `casbin_rule` table is created
// by the gorm adapter in initCasbin.
func initDB(dsn string) {
	if dsn == "" {
		dbLog.Info("DATABASE_URL not configured — DB / auth / authz endpoints will return 503")
		return
	}
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		dbLog.Warnf("Postgres init failed: %v — DB endpoints will return 503", err)
		return
	}
	// The invitations table, then the Nomos model set. AutoMigrate only ADDS:
	// it never drops, renames or narrows, and GORM's own docs warn it may still
	// change some column types and nullability. Treat every model edit as a
	// potentially irreversible production operation (docs/PLAN.md §2.4).
	models := append([]any{&types.Invitation{}}, types.NomosModels()...)
	if err := gdb.AutoMigrate(models...); err != nil {
		dbLog.Warnf("AutoMigrate(app tables) failed: %v", err)
		return
	}
	DB = gdb
	dbLog.Infof("Postgres (GORM) connected and app tables migrated (%d models)", len(models))

	if err := EnsureSearchIndexes(); err != nil {
		// Retrieval degrades to a sequential scan; everything else still
		// works. Boot rather than crash, but say so loudly.
		dbLog.Errorf("EnsureSearchIndexes failed: %v — retrieval will be slow until this is fixed", err)
	}
}

// EnsureSearchIndexes creates the two index types AutoMigrate cannot express.
//
// THIS IS THE ONE SANCTIONED PIECE OF RAW DDL IN THE CODEBASE, and
// docs/PLAN.md §9.2 is where it was agreed. The reason is narrow and factual:
// GORM's `index` tag can create a B-tree, and `type:halfvec(1536)` /
// `type:tsvector` can create the columns, but the tag vocabulary has no way to
// say `USING hnsw (embedding halfvec_cosine_ops)` or `USING gin (search_vector)`.
// Without those two access methods, hybrid retrieval (§6.2) is a sequential
// scan over the whole corpus. Smuggling arbitrary boot-time DDL into a utility
// function and calling the "AutoMigrate only" rule satisfied would be
// dishonest, so this function is deliberately a FIXED, CLOSED list:
// additive-only, idempotent, no ALTER, no DROP, no schema shape.
//
// It is safe to run on every boot only because §5.3 keeps the corpus to one
// practice area (~10^5 chunks). CREATE INDEX IF NOT EXISTS on an existing
// index is a catalogue lookup; on an EMPTY table it is instant. If the corpus
// ever grows to the full 10^7-chunk estimate, building HNSW at boot becomes
// deployment behaviour — rollout timeout, DDL contention, temporary disk
// exhaustion — and index creation must move to an explicit admin job
// (§9.1(10)). That is the tripwire: watch legal_chunks' row count.
//
// Note what is deliberately absent: there is NO HNSW index on piece_chunks.
// Private retrieval filters on tenant_id + dossier_id and then scores exactly,
// which avoids the multitenant ANN trap where HNSW returns globally-near
// neighbours that the tenant filter throws away (§6.1).
func EnsureSearchIndexes() error {
	if DB == nil {
		return ErrAuthNotConfigured
	}

	// Extensions. In production these are usually created at initdb and the
	// app user has no CREATE privilege, so a permission error here is
	// expected and must not fail the boot. A real
	// absence surfaces later, as a failing CREATE INDEX below.
	for _, ext := range []string{"vector", "unaccent"} {
		if err := DB.Exec("CREATE EXTENSION IF NOT EXISTS " + ext).Error; err != nil {
			dbLog.Warnf("CREATE EXTENSION %s skipped: %v (expected when the app user lacks CREATE — install it at initdb)", ext, err)
		}
	}

	// Public corpus: HNSW for the vector arm and GIN for the french FTS arm
	// of the RRF fusion (§6.2). halfvec_cosine_ops matches
	// text-embedding-3-small's cosine space and the halfvec(1536) column type.
	statements := []struct{ name, sql string }{
		{
			"idx_legal_chunks_embedding",
			`CREATE INDEX IF NOT EXISTS idx_legal_chunks_embedding
			   ON legal_chunks USING hnsw (embedding halfvec_cosine_ops)`,
		},
		{
			"idx_legal_chunks_fts",
			`CREATE INDEX IF NOT EXISTS idx_legal_chunks_fts
			   ON legal_chunks USING gin (search_vector)`,
		},
		// Private corpus: FTS only, on purpose. See the note above.
		{
			"idx_piece_chunks_fts",
			`CREATE INDEX IF NOT EXISTS idx_piece_chunks_fts
			   ON piece_chunks USING gin (search_vector)`,
		},
	}

	for _, st := range statements {
		if err := DB.Exec(st.sql).Error; err != nil {
			dbLog.Errorf("Search index %s failed: %v", st.name, err)
			return err
		}
	}

	dbLog.Infof("Search indexes ensured (%d: hnsw on legal_chunks.embedding, gin on both search_vectors)", len(statements))
	return nil
}
