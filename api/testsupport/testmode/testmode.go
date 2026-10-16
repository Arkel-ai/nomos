// Package testmode holds the three switches that decide what a Nomos test run
// is allowed to touch. It is a leaf package —
// standard library and godotenv only — so every test package can import it,
// including the internal tests of services and connectors that testsupport
// itself would import in a cycle.
//
//	NOMOS_LIVE=0                  deterministic suites only: every paid or
//	                              networked suite is explicitly excluded, even
//	                              when its own opt-in flag is set.
//	NOMOS_LIVE=1                  paid and networked suites run (each may still
//	                              need its own flag, e.g. NOMOS_E2E=1).
//	(unset)                       each suite's historical opt-in decides.
//	NOMOS_REQUIRE_DEPENDENCIES=1  a missing dependency (Postgres, credentials,
//	                              fixtures) FAILS the test instead of skipping
//	                              it: a gate run must never report green on a
//	                              suite that did not run.
//	NOMOS_TEST_NO_DOTENV=1        no .env file is ever loaded and no developer
//	                              default (the dev database DSN) is assumed:
//	                              the environment is the only input.
package testmode

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/joho/godotenv"
)

// The environment variables, by name.
const (
	EnvLive                = "NOMOS_LIVE"
	EnvRequireDependencies = "NOMOS_REQUIRE_DEPENDENCIES"
	EnvNoDotenv            = "NOMOS_TEST_NO_DOTENV"
)

func flag(name string) string { return strings.TrimSpace(os.Getenv(name)) }

// LiveExcluded is NOMOS_LIVE=0: no suite may spend money or reach the network.
func LiveExcluded() bool { return flag(EnvLive) == "0" }

// LiveRequested is NOMOS_LIVE=1.
func LiveRequested() bool { return flag(EnvLive) == "1" }

// RequireDependencies is NOMOS_REQUIRE_DEPENDENCIES=1.
func RequireDependencies() bool { return flag(EnvRequireDependencies) == "1" }

// NoDotenv is NOMOS_TEST_NO_DOTENV=1.
func NoDotenv() bool { return flag(EnvNoDotenv) == "1" }

// LoadDotenv loads each candidate file on its own (godotenv stops at the first
// file it cannot open), unless NOMOS_TEST_NO_DOTENV=1. Already-exported
// variables always win: godotenv never overwrites them.
func LoadDotenv(files ...string) {
	if NoDotenv() {
		return
	}
	for _, f := range files {
		_ = godotenv.Load(f)
	}
}

// Live gates a paid or networked suite. It is excluded under NOMOS_LIVE=0;
// it runs under NOMOS_LIVE=1; with NOMOS_LIVE unset it runs when one of its
// historical opt-in flags is "1", or — when it has none — as it always did.
func Live(t testing.TB, suite string, optIns ...string) {
	t.Helper()
	switch {
	case LiveExcluded():
		t.Skipf("%s excluded: NOMOS_LIVE=0 runs the deterministic suites only", suite)
	case LiveRequested():
		return
	case len(optIns) == 0:
		return
	}
	for _, name := range optIns {
		if flag(name) == "1" {
			return
		}
	}
	t.Skipf("%s not requested: set NOMOS_LIVE=1 or %s=1", suite, strings.Join(optIns, "=1 or "))
}

// Exclude marks an optional benchmark that is not part of any gate. It runs
// only on its own flag, never under NOMOS_LIVE=0, and its absence is a skip
// that names itself — never a pass.
func Exclude(t testing.TB, suite, optIn string) {
	t.Helper()
	if LiveExcluded() {
		t.Skipf("%s excluded: optional benchmark, and NOMOS_LIVE=0", suite)
	}
	if flag(optIn) != "1" {
		t.Skipf("%s excluded: optional benchmark, set %s=1 to run it", suite, optIn)
	}
}

// Missing reports an absent dependency: a failure under
// NOMOS_REQUIRE_DEPENDENCIES=1, a skip otherwise.
func Missing(t testing.TB, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if RequireDependencies() {
		t.Fatalf("required dependency missing (%s=1): %s", EnvRequireDependencies, msg)
	}
	t.Skip(msg)
}

// DatabaseURL is DATABASE_URL, or the developer default when the run allows
// developer defaults. Under NOMOS_TEST_NO_DOTENV=1 an unset DATABASE_URL stays
// empty: a gate run never falls back to a developer database.
func DatabaseURL(devDefault string) string {
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn
	}
	if NoDotenv() {
		return ""
	}
	return devDefault
}
