package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
	u "github.com/Arkel-ai/nomos/api/utils"
)

// Compétences and memory, against a REAL Postgres.
//
// The load-bearing assertion is TestSkillVersionsAreImmutable: publishing a
// change must INSERT version N+1 and leave version N byte-for-byte intact. If
// that goes red, an act drafted last March can no longer be reproduced from
// the definition that produced it, and §8's whole justification for the
// skills/skill_versions split is gone.

// skillFixture is a tenant of its own, so nothing here can observe or disturb
// another suite's rows.
type skillFixture struct {
	firm string
	user string
}

func newSkillFixture(t *testing.T) *skillFixture {
	t.Helper()
	setupConversationSuite()
	if !dbReady {
		testmode.Missing(t, "Postgres/Casbin not reachable — skills suite skipped")
	}
	f := &skillFixture{
		firm: "test_firm_skill_" + uuid.NewString(),
		user: "test_user_skill_" + uuid.NewString(),
	}
	if err := u.EnsureFirm(context.Background(), f.firm, "Cabinet de test", f.user); err != nil {
		t.Fatalf("EnsureFirm: %v", err)
	}
	if err := u.AddRoleForUserInDomain(f.user, types.RoleUser, f.firm); err != nil {
		t.Fatalf("attach user: %v", err)
	}
	t.Cleanup(func() {
		db := u.DB
		for _, table := range []string{
			"skill_versions", "skills", "memory_preferences", "audit_events",
		} {
			db.Exec(fmt.Sprintf("DELETE FROM %s WHERE tenant_id = ?", table), f.firm)
		}
		db.Exec("DELETE FROM casbin_rule WHERE v1 = ? OR v2 = ?", f.firm, f.firm)
		db.Exec("DELETE FROM firms WHERE id = ?", f.firm)
		db.Exec("DELETE FROM casbin_rule WHERE v0 = ?", f.user)
		if err := u.Enforcer.LoadPolicy(); err != nil {
			t.Logf("reloading casbin policy: %v", err)
		}
	})
	return f
}

func definitionJSON(t *testing.T, sectionTitle string) types.JSONB {
	t.Helper()
	b, err := json.Marshal(skillDefinition{
		Sections: []skillSection{{Title: sectionTitle, Purpose: "Objet de la section.", Required: true}},
		Tone:     skillTone{Register: "soutenu", Person: "3e personne", Notes: "Style impersonnel."},
		MandatoryClauses: []skillClause{
			{Label: "Réserve", Text: "Sous toutes réserves.", Why: "Préserve les demandes non formulées."},
		},
		Checklist: []skillCheck{{Item: "Les dates sont vérifiées.", Blocking: true}},
	})
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}
	return types.JSONB(b)
}

func publish(t *testing.T, f *skillFixture, slug, name, sectionTitle string) types.SkillDetail {
	t.Helper()
	status, out := CreateSkill(ctxFor(f.firm, f.user), types.SkillCreateRequest{
		Slug:       slug,
		Name:       name,
		Definition: definitionJSON(t, sectionTitle),
	})
	if status != http.StatusCreated {
		t.Fatalf("CreateSkill: status %d body %v", status, out)
	}
	detail, ok := out.(types.SkillDetail)
	if !ok {
		t.Fatalf("CreateSkill returned %T, want types.SkillDetail", out)
	}
	return detail
}

// TestSkillVersionsAreImmutable is the §8 guarantee, asserted at the row level.
func TestSkillVersionsAreImmutable(t *testing.T) {
	f := newSkillFixture(t)
	slug := "conclusions-prudhomales"

	v1 := publish(t, f, slug, "Conclusions prud'homales", "Exposé des faits")
	if v1.CurrentVersion != 1 || len(v1.Versions) != 1 {
		t.Fatalf("first publication: current_version=%d versions=%d", v1.CurrentVersion, len(v1.Versions))
	}
	first := v1.Versions[0]
	firstDefinition := string(first.Definition)

	// Publishing a change to the SAME slug is a new version, never an edit.
	v2 := publish(t, f, slug, "Conclusions prud'homales (charte 2026)", "Rappel de la procédure")
	if v2.ID != v1.ID {
		t.Fatalf("identity changed: %s → %s — a new version must not fork the skill", v1.ID, v2.ID)
	}
	if v2.CurrentVersion != 2 {
		t.Fatalf("current_version = %d after the second publication, want 2", v2.CurrentVersion)
	}
	if len(v2.Versions) != 2 {
		t.Fatalf("history holds %d versions, want 2 — a version was overwritten", len(v2.Versions))
	}
	if v2.Versions[0].Version != 2 || v2.Versions[1].Version != 1 {
		t.Fatalf("history is not newest-first: %d, %d", v2.Versions[0].Version, v2.Versions[1].Version)
	}

	// Version 1 must be byte-for-byte what it was, in the database.
	repo := u.ForTenantID(context.Background(), f.firm)
	var stored types.SkillVersion
	if err := repo.SkillVersions().Where("id = ?", first.ID).Take(&stored).Error; err != nil {
		t.Fatalf("re-reading version 1: %v", err)
	}
	if string(stored.Definition) != firstDefinition {
		t.Fatalf("version 1 definition was mutated:\n before: %s\n after:  %s", firstDefinition, stored.Definition)
	}
	if stored.CreatedAt != first.CreatedAt || stored.Version != 1 || stored.SkillID != v1.ID {
		t.Fatalf("version 1 row was rewritten: %+v", stored)
	}
	if string(v2.Versions[0].Definition) == firstDefinition {
		t.Fatal("version 2 carries version 1's definition — the new definition was not stored")
	}

	// Archiving keeps the history: a pleading built on version 1 stays
	// reproducible after the compétence is retired.
	status, out := DeleteSkill(ctxFor(f.firm, f.user), v1.ID)
	if status != http.StatusOK {
		t.Fatalf("DeleteSkill: status %d body %v", status, out)
	}
	var remaining int64
	if err := repo.SkillVersions().Where("skill_id = ?", v1.ID).Count(&remaining).Error; err != nil {
		t.Fatalf("counting versions after archive: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("%d versions survive the archive, want 2", remaining)
	}
	status, out = GetSkill(ctxFor(f.firm, f.user), v1.ID)
	if status != http.StatusOK {
		t.Fatalf("GetSkill after archive: status %d body %v", status, out)
	}
	if out.(types.SkillDetail).State != types.SkillStateArchived {
		t.Fatalf("archived skill state = %s", out.(types.SkillDetail).State)
	}
}

// TestSkillBuiltinsAndScoping covers the two other things the library promises:
// the composer's slash commands exist as real versioned rows, and one firm's
// compétences never reach another.
func TestSkillBuiltinsAndScoping(t *testing.T) {
	f := newSkillFixture(t)
	other := newSkillFixture(t)

	status, out := ListSkills(ctxFor(f.firm, f.user), types.ListQuery{})
	if status != http.StatusOK {
		t.Fatalf("ListSkills: status %d body %v", status, out)
	}
	rows := out.(types.ListResponse).Items.([]types.Skill)
	bySlug := map[string]types.Skill{}
	for _, s := range rows {
		bySlug[s.Slug] = s
	}
	for _, b := range builtinSkills() {
		s, ok := bySlug[b.Slug]
		if !ok {
			t.Fatalf("built-in /%s is missing from the library", b.Slug)
		}
		if s.CurrentVersion != 1 {
			t.Fatalf("built-in /%s current_version = %d, want 1", b.Slug, s.CurrentVersion)
		}
	}

	// Seeding is idempotent — a second page load must not fork a version.
	if status, out = ListSkills(ctxFor(f.firm, f.user), types.ListQuery{}); status != http.StatusOK {
		t.Fatalf("second ListSkills: status %d body %v", status, out)
	}
	if got := out.(types.ListResponse).Total; got != int64(len(builtinSkills())) {
		t.Fatalf("library holds %d compétences after two loads, want %d", got, len(builtinSkills()))
	}

	// A firm's own compétence is invisible to another tenant.
	own := publish(t, f, "trame-du-cabinet", "Trame du cabinet", "Objet")
	status, _ = GetSkill(ctxFor(other.firm, other.user), own.ID)
	if status != http.StatusNotFound {
		t.Fatalf("cross-tenant GetSkill returned %d, want 404", status)
	}
	status, out = ListSkills(ctxFor(other.firm, other.user), types.ListQuery{})
	if status != http.StatusOK {
		t.Fatalf("other-tenant ListSkills: status %d body %v", status, out)
	}
	for _, s := range out.(types.ListResponse).Items.([]types.Skill) {
		if s.Slug == own.Slug {
			t.Fatal("a compétence leaked across tenants")
		}
	}
}

// --- the CPH requête compétence (owner decision 5) ---------------------------

const cphSlug = "requete-cph-resiliation-judiciaire"

func builtinCPH(t *testing.T) builtinSkill {
	t.Helper()
	for _, b := range builtinSkills() {
		if b.Slug == cphSlug {
			return b
		}
	}
	t.Fatalf("built-in /%s is not defined", cphSlug)
	return builtinSkill{}
}

// A firm seeded by an earlier release already holds the other built-ins: the
// CPH requête must still reach it, once, without touching what it has.
func TestSkillBuiltinCPHReachesExistingTenantsOnce(t *testing.T) {
	f := newSkillFixture(t)
	repo := u.ForTenantID(context.Background(), f.firm)
	if status, out := ListSkills(ctxFor(f.firm, f.user), types.ListQuery{}); status != http.StatusOK {
		t.Fatalf("ListSkills: status %d body %v", status, out)
	}

	// Rewind the firm to what the previous release seeded.
	var first types.Skill
	if err := repo.Skills().Where("slug = ?", cphSlug).Take(&first).Error; err != nil {
		t.Fatalf("a new firm was not seeded with /%s: %v", cphSlug, err)
	}
	u.DB.Exec("DELETE FROM skill_versions WHERE tenant_id = ? AND skill_id = ?", f.firm, first.ID)
	u.DB.Exec("DELETE FROM skills WHERE tenant_id = ? AND id = ?", f.firm, first.ID)
	var older []types.Skill
	if err := repo.Skills().Find(&older).Error; err != nil {
		t.Fatalf("listing the earlier built-ins: %v", err)
	}
	if len(older) != len(builtinSkills())-1 {
		t.Fatalf("rewound firm holds %d compétences, want %d", len(older), len(builtinSkills())-1)
	}

	for i := 0; i < 2; i++ {
		if status, out := ListSkills(ctxFor(f.firm, f.user), types.ListQuery{}); status != http.StatusOK {
			t.Fatalf("ListSkills #%d: status %d body %v", i+1, status, out)
		}
	}

	var rows []types.Skill
	if err := repo.Skills().Where("slug = ?", cphSlug).Find(&rows).Error; err != nil {
		t.Fatalf("reading /%s: %v", cphSlug, err)
	}
	if len(rows) != 1 {
		t.Fatalf("/%s exists %d times after two loads, want once", cphSlug, len(rows))
	}
	cph := rows[0]
	if cph.State != types.SkillStateActive || cph.CurrentVersion != 1 || cph.CreatedBy != skillExtractorBuiltin {
		t.Fatalf("/%s seeded as state=%s version=%d by=%s", cphSlug, cph.State, cph.CurrentVersion, cph.CreatedBy)
	}
	if !strings.HasPrefix(cph.Description, "Projet à valider par l'avocat") {
		t.Fatalf("the draft is not marked as awaiting the lawyer's validation: %q", cph.Description)
	}
	var versions []types.SkillVersion
	if err := repo.SkillVersions().Where("skill_id = ?", cph.ID).Find(&versions).Error; err != nil {
		t.Fatalf("reading /%s versions: %v", cphSlug, err)
	}
	if len(versions) != 1 || versions[0].Version != 1 || versions[0].ExtractorModel != skillExtractorBuiltin {
		t.Fatalf("/%s versions: %+v", cphSlug, versions)
	}
	var stored skillDefinition
	if err := json.Unmarshal(versions[0].Definition, &stored); err != nil {
		t.Fatalf("stored definition: %v", err)
	}
	if !reflect.DeepEqual(stored, builtinCPH(t).Definition) {
		t.Fatal("the stored definition is not the built-in one")
	}

	for _, s := range older {
		var now types.Skill
		if err := repo.Skills().Where("slug = ?", s.Slug).Take(&now).Error; err != nil {
			t.Fatalf("/%s vanished: %v", s.Slug, err)
		}
		var n int64
		if err := repo.SkillVersions().Where("skill_id = ?", s.ID).Count(&n).Error; err != nil {
			t.Fatalf("counting /%s versions: %v", s.Slug, err)
		}
		if now.ID != s.ID || now.CurrentVersion != 1 || n != 1 {
			t.Fatalf("/%s was touched: id %s→%s version=%d versions=%d", s.Slug, s.ID, now.ID, now.CurrentVersion, n)
		}
	}
}

// The agent finds it with list_competences, and once the lawyer picks it,
// load_competence hands over that pinned version's drafting constraints.
func TestCompetenceCPHExposedToTheAgent(t *testing.T) {
	f := newToolFixture(t)
	if err := ensureBuiltinSkills(f.repo()); err != nil {
		t.Fatalf("ensureBuiltinSkills: %v", err)
	}
	b := builtinCPH(t)
	c := f.contract()

	listed := f.call(f.exec(c), types.ToolListCompetences, nil)
	wantOK(t, listed)
	var list struct {
		Competences []struct {
			Slug           string `json:"slug"`
			Name           string `json:"name"`
			Description    string `json:"description"`
			CurrentVersion int    `json:"current_version"`
		} `json:"competences"`
	}
	if err := json.Unmarshal(listed.Observation.Data, &list); err != nil {
		t.Fatalf("list_competences data: %v", err)
	}
	found := false
	for _, s := range list.Competences {
		if s.Slug == cphSlug {
			found = s.Name == b.Name && s.Description == b.Description && s.CurrentVersion == 1
		}
	}
	if !found {
		t.Fatalf("list_competences does not offer /%s as built: %s", cphSlug, listed.Observation.Data)
	}

	pin, ok := pinCompetence(f.repo(), cphSlug)
	if !ok {
		t.Fatalf("the lawyer's pick of /%s does not pin", cphSlug)
	}
	c.Competence = pin
	loaded := f.call(f.exec(c), types.ToolLoadCompetence, map[string]any{"slug": cphSlug})
	wantOK(t, loaded)
	var got struct {
		SkillVersionID   string         `json:"skill_version_id"`
		Version          int            `json:"version"`
		Pinned           bool           `json:"pinned"`
		Sections         []skillSection `json:"sections"`
		Tone             skillTone      `json:"tone"`
		MandatoryClauses []skillClause  `json:"mandatory_clauses"`
		Checklist        []skillCheck   `json:"checklist"`
	}
	if err := json.Unmarshal(loaded.Observation.Data, &got); err != nil {
		t.Fatalf("load_competence data: %v", err)
	}
	if got.SkillVersionID != pin.SkillVersionID || got.Version != 1 || !got.Pinned {
		t.Fatalf("load_competence is not the pinned version: %+v vs %+v", got, pin)
	}
	if !reflect.DeepEqual(skillDefinition{Sections: got.Sections, Tone: got.Tone, MandatoryClauses: got.MandatoryClauses, Checklist: got.Checklist}, b.Definition) {
		t.Fatalf("load_competence does not carry the built-in definition: %s", loaded.Observation.Data)
	}
	// Small enough to reach the controller whole, never replaced by a stub.
	if n := utf8.RuneCount(loaded.Observation.Data); n > agentObservationChars/2 {
		t.Fatalf("load_competence observation is %d chars, too close to the %d cap", n, agentObservationChars)
	}
}

// The verification gate holds a CPH draft to the compétence: a complete draft
// passes the compétence check, a missing section or mention is named.
func TestCompetenceCPHVerificationGate(t *testing.T) {
	f := newAgentFixture(t, "cph")
	t.Cleanup(func() {
		u.DB.Exec("DELETE FROM skill_versions WHERE tenant_id = ?", f.firm)
		u.DB.Exec("DELETE FROM skills WHERE tenant_id = ?", f.firm)
	})
	f.must(ensureBuiltinSkills(f.repo))
	pin, ok := pinCompetence(f.repo, cphSlug)
	if !ok {
		t.Fatalf("/%s does not pin", cphSlug)
	}
	f.contract.Competence = pin
	f.contract.OutputKinds = append(f.contract.OutputKinds, types.AgentOutputActe)
	f.saveContract()

	// Headings the way a requête writes them: capitals, numbering, and words
	// around the compétence's titles.
	headings := []string{
		"JURIDICTION SAISIE", "DÉSIGNATION DES PARTIES", "OBJET DE LA DEMANDE", "I. EXPOSÉ DES FAITS",
		"II. MOYENS", "PAR CES MOTIFS", "BORDEREAU DE PIÈCES COMMUNIQUÉES",
	}
	draft := func(skipHeading string, signed bool) types.AgentDocumentRevision {
		var blocks []types.DocumentBlock
		for i, h := range headings {
			if h == skipHeading {
				continue
			}
			blocks = append(blocks,
				types.DocumentBlock{ID: fmt.Sprintf("h%d", i), Role: types.BlockHeading, Level: 1, Text: h},
				para(fmt.Sprintf("p%d", i), "Contenu de la section."))
		}
		blocks = append(blocks, para("d1", "PRONONCER la résiliation judiciaire du contrat de travail aux torts de l'employeur."))
		if signed {
			blocks = append(blocks, para("d2", "Fait à Lyon."))
		}
		return f.addRevision(types.AgentOutputActe, "Requête", blocks...)
	}
	competenceIssues := func(r types.VerificationReport) []types.VerificationIssue {
		var out []types.VerificationIssue
		for _, iss := range r.Issues {
			if iss.Code == types.IssueCompetenceSection || iss.Code == types.IssueCompetenceClause {
				out = append(out, iss)
			}
		}
		return out
	}

	complete := f.verify(draft("", true).ID, false)
	if checkStatus(complete, types.CheckCompetence) != types.CheckPassed || len(competenceIssues(complete)) != 0 {
		t.Fatalf("a complete CPH draft must pass the compétence check: %s %+v", checkStatus(complete, types.CheckCompetence), competenceIssues(complete))
	}

	noMoyens := f.verify(draft("II. MOYENS", true).ID, false)
	issues := competenceIssues(noMoyens)
	if noMoyens.Result != types.VerificationBlocked || len(issues) != 1 ||
		issues[0].Code != types.IssueCompetenceSection || !strings.Contains(issues[0].Correction, "« Moyens »") {
		t.Fatalf("a draft without its Moyens must be blocked on that section: %s %+v", noMoyens.Result, issues)
	}

	unsigned := f.verify(draft("", false).ID, false)
	issues = competenceIssues(unsigned)
	if unsigned.Result != types.VerificationBlocked || len(issues) != 1 ||
		issues[0].Code != types.IssueCompetenceClause || !strings.Contains(issues[0].Correction, "Fait à") {
		t.Fatalf("an undated, unsigned draft must be blocked on that mention: %s %+v", unsigned.Result, issues)
	}
}

// TestMemoryProvenanceAndDeletion asserts §8's consent rule: an inferred entry
// is never usable until a human approves it, and deletion is real.
func TestMemoryProvenanceAndDeletion(t *testing.T) {
	f := newSkillFixture(t)
	other := newSkillFixture(t)

	inferred := types.MemoryUpsertRequest{
		Scope:      types.MemoryScopeUser,
		Key:        "formule_de_politesse",
		Value:      types.JSONB(`{"text":"Je vous prie d'agréer"}`),
		Provenance: types.MemoryProvenanceInferred,
	}
	status, out := UpsertMemory(ctxFor(f.firm, f.user), inferred)
	if status != http.StatusOK {
		t.Fatalf("UpsertMemory(inferred): status %d body %v", status, out)
	}
	entry := out.(types.MemoryPreference)
	if entry.State != types.MemoryStateProposed || entry.ApprovedAt != nil {
		t.Fatalf("an inferred preference was stored as state=%s approved_at=%v — it must wait for consent",
			entry.State, entry.ApprovedAt)
	}

	// Approval is a human POST, and it replaces the same row rather than
	// growing a second one the user has to audit.
	approved := inferred
	approved.Provenance = types.MemoryProvenanceApproved
	status, out = UpsertMemory(ctxFor(f.firm, f.user), approved)
	if status != http.StatusOK {
		t.Fatalf("UpsertMemory(approved): status %d body %v", status, out)
	}
	promoted := out.(types.MemoryPreference)
	if promoted.ID != entry.ID {
		t.Fatalf("approval forked the entry: %s → %s", entry.ID, promoted.ID)
	}
	if promoted.State != types.MemoryStateApproved || promoted.ApprovedAt == nil {
		t.Fatalf("approved preference state=%s approved_at=%v", promoted.State, promoted.ApprovedAt)
	}

	status, out = ListMemory(ctxFor(f.firm, f.user), types.ListQuery{})
	if status != http.StatusOK {
		t.Fatalf("ListMemory: status %d body %v", status, out)
	}
	if got := out.(types.ListResponse).Total; got != 1 {
		t.Fatalf("memory holds %d entries after an approval, want 1", got)
	}

	// One firm's memory never reaches another.
	status, out = ListMemory(ctxFor(other.firm, other.user), types.ListQuery{})
	if status != http.StatusOK {
		t.Fatalf("other-tenant ListMemory: status %d body %v", status, out)
	}
	if got := out.(types.ListResponse).Total; got != 0 {
		t.Fatalf("another firm sees %d memory entries, want 0", got)
	}
	if status, _ = DeleteMemory(ctxFor(other.firm, other.user), entry.ID); status != http.StatusNotFound {
		t.Fatalf("cross-tenant DeleteMemory returned %d, want 404", status)
	}

	// Deletion is real.
	if status, out = DeleteMemory(ctxFor(f.firm, f.user), entry.ID); status != http.StatusOK {
		t.Fatalf("DeleteMemory: status %d body %v", status, out)
	}
	status, out = ListMemory(ctxFor(f.firm, f.user), types.ListQuery{})
	if status != http.StatusOK {
		t.Fatalf("ListMemory after delete: status %d body %v", status, out)
	}
	if got := out.(types.ListResponse).Total; got != 0 {
		t.Fatalf("%d entries survive the deletion, want 0", got)
	}
}

// The agent can be the first reader of a tenant's compétences (a turn sent
// before the composer ever listed them): list_competences and load_competence
// seed the built-ins themselves, so the CPH compétence is there to offer.
func TestCompetenceToolsSeedTheBuiltins(t *testing.T) {
	f := newToolFixture(t)
	var before int64
	if err := f.repo().Skills().Count(&before).Error; err != nil || before != 0 {
		t.Fatalf("fixture tenant should start without compétences: %d %v", before, err)
	}
	listed := f.call(f.exec(f.contract()), types.ToolListCompetences, nil)
	wantOK(t, listed)
	if !strings.Contains(string(listed.Observation.Data), cphSlug) {
		t.Fatalf("list_competences on an unseeded tenant does not offer /%s: %s", cphSlug, listed.Observation.Data)
	}

	g := newToolFixture(t)
	loaded := g.call(g.exec(g.contract()), types.ToolLoadCompetence, map[string]any{"slug": cphSlug})
	wantOK(t, loaded)
}
