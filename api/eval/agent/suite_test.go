package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Arkel-ai/nomos/api/testsupport/testmode"
	"github.com/Arkel-ai/nomos/api/types"
)

func loadAll(t *testing.T) (Manifest, Suite, Suite) {
	t.Helper()
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	fr, err := LoadSuite(SuiteFR, m)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := LoadSuite(SuiteAgentFailures, m)
	if err != nil {
		t.Fatal(err)
	}
	return m, fr, ag
}

// The matrix is exactly 12 × 6, the agent suite exactly 12, and each tier
// plans its fixed workload — never a subset.
func TestSuitesHaveTheirFixedShape(t *testing.T) {
	_, fr, ag := loadAll(t)
	if len(fr.Cases) != 72 || len(ag.Cases) != 12 {
		t.Fatalf("fr_v1 has %d cases, agent_failures_v1 %d; want 72 and 12", len(fr.Cases), len(ag.Cases))
	}
	for tier, want := range map[string]int{TierFast: 60, TierFull: 396} {
		trials, err := PlanTrials(tier, &fr, &ag, "")
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		if len(trials) != want {
			t.Fatalf("%s plans %d trials, want %d", tier, len(trials), want)
		}
		seen := map[string]bool{}
		for _, tr := range trials {
			if seen[tr.ID] {
				t.Fatalf("%s: trial %s planned twice", tier, tr.ID)
			}
			seen[tr.ID] = true
		}
	}
	if _, err := PlanTrials(TierFast, &fr, nil, ""); err == nil {
		t.Fatal("a fast run without the agent suite must be refused")
	}
	if _, err := PlanTrials(TierFull, &fr, &ag, "c05-s1"); err == nil {
		t.Fatal("-case must not narrow a full run")
	}
	if _, err := PlanTrials(TierSmoke, &fr, nil, ""); err == nil {
		t.Fatal("a smoke run needs -case")
	}
	smoke, err := PlanTrials(TierSmoke, &fr, nil, "c05-s1")
	if err != nil || len(smoke) != 1 || smoke[0].Case.Incident != IncidentChronology {
		t.Fatalf("smoke c05-s1 = %v, %v", smoke, err)
	}
}

// Both 15 October incidents are in the matrix word for word, in the fast
// tier, and the CPH incident is played without and with a compétence.
func TestIncidentsAreWordForWord(t *testing.T) {
	_, fr, _ := loadAll(t)
	var chrono, cphWithout, cphWith int
	for _, c := range fr.Cases {
		switch c.Incident {
		case IncidentChronology:
			chrono++
			if c.Instruction != ChronologyInstruction || c.Dossier != "vasseur" || !c.Fast {
				t.Fatalf("%s is not the exact chronology incident on Vasseur", c.ID)
			}
			if c.Oracle.GoldEvents == "" || !c.Oracle.CompleteCoverage || !c.Oracle.DOCXFidelity {
				t.Fatalf("%s: the chronology incident needs gold events, full coverage and DOCX fidelity", c.ID)
			}
		case IncidentCPH:
			if c.Instruction != CPHInstruction || c.Dossier != "benali" {
				t.Fatalf("%s is not the exact CPH incident on the judicial-termination fixture", c.ID)
			}
			if c.Competence == "" {
				cphWithout++
			} else {
				cphWith++
				if c.Answers == nil {
					t.Fatalf("%s: the pinned-compétence variant needs material lawyer answers", c.ID)
				}
			}
		}
	}
	if chrono != 1 || cphWithout != 1 || cphWith != 1 {
		t.Fatalf("incidents: chronology %d, CPH without %d, with %d — want 1 each", chrono, cphWithout, cphWith)
	}
}

// A suite that breaks the matrix, asks for an acte without a compétence or
// carries a fault the proxy cannot play is never run.
func TestValidationRefusesBrokenSuites(t *testing.T) {
	m, fr, ag := loadAll(t)
	mutate := func(s Suite, f func(*Suite)) error {
		cp := s
		cp.Cases = append([]Case(nil), s.Cases...)
		f(&cp)
		return ValidateSuite(cp, m)
	}
	cases := map[string]error{
		"a missing cell": mutate(fr, func(s *Suite) { s.Cases = s.Cases[1:] }),
		"a duplicated cell": mutate(fr, func(s *Suite) {
			c := s.Cases[1]
			c.ID = "c99-dup"
			c.Class, c.State = s.Cases[0].Class, s.Cases[0].State
			s.Cases[1] = c
		}),
		"a reworded incident": mutate(fr, func(s *Suite) {
			for i := range s.Cases {
				if s.Cases[i].Incident == IncidentChronology {
					s.Cases[i].Instruction = strings.TrimSuffix(s.Cases[i].Instruction, ".")
				}
			}
		}),
		"an acte without compétence": mutate(fr, func(s *Suite) {
			s.Cases[0].Oracle.RequireDocuments = []string{types.AgentOutputActe}
		}),
		"an unknown fault": mutate(ag, func(s *Suite) {
			s.Cases[0].Faults = []Fault{{Dispatch: 1, Kind: "drop_usage"}}
		}),
		"a fault on no dispatch": mutate(ag, func(s *Suite) {
			s.Cases[0].Faults = []Fault{{Kind: FaultHTTPError, Status: 503}}
		}),
		"a raised dispatch ceiling": mutate(ag, func(s *Suite) { s.Cases[0].Oracle.ExactDispatches = 41 }),
		"an unknown gold list":      mutate(fr, func(s *Suite) { s.Cases[0].Oracle.GoldEvents = "nope" }),
		"a pièce outside the dossier": mutate(fr, func(s *Suite) {
			s.Cases[0].Oracle.RequirePieces = []string{"99-inexistante.pdf"}
		}),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Every pinned fixture is present with its exact bytes. The fixtures are
// untracked: without them this is a skip, and under
// NOMOS_REQUIRE_DEPENDENCIES=1 a loud failure — never a silent pass.
func TestManifestPinsTheFixtures(t *testing.T) {
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	root := firstNonEmpty(os.Getenv("NOMOS_EVAL_TESTDATA"), filepath.Join("..", "..", "..", "testdata"))
	if _, err := os.Stat(root); err != nil {
		testmode.Missing(t, "testdata/ fixtures are absent (%s): they are untracked by design", root)
	}
	var all []string
	for k := range m.Dossiers {
		all = append(all, k)
	}
	if problems := VerifyFixtures(m, root, all); len(problems) > 0 {
		t.Fatalf("%d fixture problem(s), first: %s (%s)", len(problems), problems[0].Path, problems[0].Reason)
	}
}

// A changed or missing fixture is named, not skipped.
func TestChangedFixtureIsLoud(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "a.pdf"), []byte("changed"), 0o644)
	m := Manifest{Dossiers: map[string]DossierFixture{"x": {Dir: "d", Files: []FixtureFile{
		{Name: "a.pdf", SHA256: sha256Hex([]byte("original"))},
		{Name: "b.pdf", SHA256: sha256Hex([]byte("b"))},
	}}}}
	problems := VerifyFixtures(m, dir, []string{"x"})
	if len(problems) != 2 || problems[0].Reason != "sha256 differs from the manifest" || problems[1].Reason != "missing" {
		t.Fatalf("problems = %+v", problems)
	}
}
