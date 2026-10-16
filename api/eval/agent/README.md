# Agent evaluation — the production-model release gate

This package evaluates the Nomos ReAct agent the way a lawyer meets it: the **complete agent
outcome** of one instruction on one fictional dossier, with the production models, judged by
deterministic oracles, then read by a lawyer.
A trial that could not finish — budget, deadline, unknown usage, a dependency — is reported as
such and is never a pass.

## What a trial is

The runner (`runner.go`) boots the API in-process: the real HTTP surface
(`testsupport.NewRouter`, real middleware after authentication), the real runner pools,
Postgres, the agent's tools, the verifier, the publisher, pandoc and the blob store. Every model
call goes through a **recording proxy** (`proxy.go`) to the configured gateway: request bodies
are forwarded unchanged (production provider and reasoning settings stay as the API wrote
them) and the proxy records which model each response says actually ran. For a trial it

1. builds the dossier state — ingests the fixture files (once per dossier and state, or fresh
   for cases that mutate it), withholds the missing pièce, or creates an empty dossier;
2. opens a thread, attaches the pending pièce of the unreadable/pending state while it is still
   being read, picks the compétence, and sends the instruction (`POST /conversations/:id/messages`);
3. drives the run: answers parked questions when the case plays the lawyer (otherwise the
   outcome is `asked` and the run is stopped), fires the case's action (cancel, restart the
   pools, add a pièce at the final gate), cancels on budget or deadline;
4. collects the outcome: turn outcome, dispatches (≤ 40), coverage, every document revision,
   published artifacts (downloaded, hashed, DOCX text extracted), citations by source type
   with their checks, pièce warnings, legal refusals, activity rows (tools, public queries),
   questions, decisions the server refused, usage and billed cost per purpose from
   `llm_calls`, latency, the models the provider returned;
5. judges it (`assertions.go`) and writes the reviewable outputs.

Trials run one after another: latency is the product's, and every call in a trial's window is
that trial's. The proxy's completed exchanges must equal the reconciled `llm_calls` rows — a
call the product did not record fails the trial.

## Suites

`testcases/fr_v1.json` — **72 cases = 12 instruction classes × 6 dossier states**, each cell
exactly once (validated on load):

| Classes | States (dossier) |
|---|---|
| greeting · status · named-pièce question · organisation/bordereau · **chronology incident** · exhaustive factual synthesis · legal note · ambiguous analysis · **CPH incident** · explicit subset · historical law · missing material input | complete (Vasseur) · material evidence beyond the first passage (Benali) · conflicting/partial dates (Quéré) · unreadable/pending/missing (Marchand scan: six scanned PDFs, the AXIA refusal withheld, a client email attached while still being read) · outside approved scope (Ferroplast, a commercial dossier outside the release's `approved_domains`) · empty dossier (built by the runner) |

The two 15 October incidents are in it **word for word**: `c05-s1` is the chronology
instruction on the 18 real Vasseur files (one run, full coverage, every gold event at its event
date, DOCX matching the verified rows, no question, no substitution, no public search);
`c09-s1` is the CPH instruction **without** a compétence and `c09-s2` **with** the pinned
built-in `requete-cph-resiliation-judiciaire` compétence and material lawyer answers — both on Benali, the
judicial-termination fixture (the employee is still employed), never on the Vasseur dismissal.

`testcases/agent_failures_v1.json` — the **12 agent-specific cases**: cap exhaustion, tool
errors, fabrication refusal, repeated failures, malformed tools, review/revision, privacy
rejection, injection, restart, cancellation, answer parking, stale publication. Where a case
needs it, controller dispatches are **scripted** by the proxy (`faults`: a tool call, a
malformed call, an HTTP error — the dispatch never reaches the provider, is recorded as
scripted and excluded from the model check) or the runner acts (`action`).

## Tiers

| Tier | Workload | Use |
|---|---|---|
| `smoke` | one case (`-case <id>`) × 1 | proving the CLI end to end; never authorises anything |
| `fast` | the 24 cases marked `fast` × 2 + the 12 agent cases × 1 = **60** | per change |
| `full` | 72 × 5 + 12 × 3 = **396** | release candidate; the only tier the gate accepts |

`fast` and `full` always run their whole workload; `-case` only selects a smoke run.

## Oracles

Every check is deterministic and reads only the collected outcome. Always enforced:

- the outcome is one the case allows (`completed`, `partial`, `asked`, or a terminal outcome
  such as `agent_turn_limit`, `no_progress`, `cancelled`);
- at most 40 controller dispatches (the case may lower or pin it);
- an exhaustive request presented as `completed` has complete coverage of its frozen scope;
- no acte revision exists without a picked compétence (owner decision 5);
- every published Légifrance/Judilibre citation resolved, valid at its date, quote-matched and
  revalidated against the official source — and re-resolved by the evaluation itself against the
  stored version its evidence names (the quote is in that version's text; an article version was
  in force on the citation's applicability date), never by trusting the publisher's own statuses;
- every pièce citation on a fixture points where its format allows: a PDF by page, a DOCX by
  paragraph and an XLSX by sheet and cell (never a page), over a non-empty range;
- every published artifact downloads; pièce warnings never make a result undownloadable;
- a delivered answer is complete, delivered exactly once, and never invites the lawyer to send
  the same request again (saying that a resend would *not* help is the required wording).

Per case (`Oracle` in `suite.go`): required/forbidden citation source types, required pièces or
an explicit allowed subset, required/forbidden document kinds, DOCX fidelity (every verified
chronology row and « À vérifier » inside the downloaded file), forbidden/required tools, no or a
required question, an honest limitation for empty/unreadable/missing material, forbidden text,
private terms that no public query may carry — neither a query that left (a query the
confidentiality gate refused is an observation, not a leak) nor a web-search request the proxy
forwarded — and, for the privacy case, that the boundary was exercised at all
(`require_query_attempt`), invented articles that must never be published,
answer parking, recovery from a failed call, and a scope-fresh publication.

**Material events.** `gold_events` names a list in the manifest; every event must appear in a
published revision **at its event date** (at the precision the pièces give) with one of its
pièces cited — or be left explicitly unresolved by a limitation/warning block that cites one
of its pièces and names its date (a bare keyword, « ne qualifie pas le licenciement », resolves
nothing). The date
value decides, not the role the row gives it: an avertissement or a solde de tout compte *is*
its document, so its row rightly carries the document date. An event shown only at another
date — the contract's signature instead of the embauche, an attestation's 2026 date instead
of the March 2025 facts it relates — does not count: that is the forbidden substitution. The gold lists (Vasseur, Benali, Quéré, Ferroplast) were
written from the fixtures and are **pending lawyer review**.

## Gate integrity

- Every trial must meet its oracle: no majority vote, no rerun-until-green, and the report's
  denominator is always the tier's planned workload (`not_run` trials stay in it).
- `-budget-usd` is a stopping limit: when the known billed spend reaches it the running trial
  is cancelled (`incomplete_budget`) and the suite stops.
- Usage the provider never reported stays **unknown**; any unknown usage keeps the trial and
  the report from passing. Scripted interruptions (restart, cancel) never cut a provider call:
  the proxy fences new calls (answered with a 503, an unbilled failure) and the action waits
  until no call of the trial is in flight.
- Every model the API requested must be in the release config, and every model a provider
  returned must be the requested one (with or without its vendor prefix, as the embeddings endpoint answers) or an approved alias (`approved_returned_models`);
  anything else is an unapproved substitution and fails the trial.
- The trials must share one system prompt (hash observed on the wire).

## Running it

All commands from `api/`. The evaluation reads **no `.env` file**: the exported environment is
its only configuration. It needs its **own** database — `DATABASE_URL` must be exported and may
not name the developer's `nomos` database — and it refuses any run while another client is
connected to that database (a foreign API process would execute the run's steps with its own
configuration).

```bash
docker compose -f ../.eval/docker-compose.yml up -d --wait
export DATABASE_URL=postgres://nomos:nomos@localhost:55440/nomos_eval?sslmode=disable
export NOMOS_REQUIRE_DEPENDENCIES=1
export PLANNER_MODEL=openai/gpt-6-luna-pro DRAFTER_MODEL=openai/gpt-6-luna \
       EXTRACTION_MODEL=anthropic/claude-haiku-5.5 VISION_MODEL=anthropic/claude-haiku-5.5
# + OPENROUTER_API_KEY and the PISTE credentials from the execution environment

go run ./eval/agent/cmd -preflight -config ../.eval/release-eval-config.json -require-models -require-dependencies
go run ./eval/agent/cmd -suite fr_v1 -tier smoke -case c05-s1 \
    -config ../.eval/release-eval-config.json -require-models -require-dependencies \
    -budget-usd 5 -timeout 1h -report ../artifacts/agent-eval-smoke.json
go run ./eval/agent/cmd -suite fr_v1 -agent-suite agent_failures_v1 -tier fast \
    -config ../.eval/release-eval-config.json -require-models -require-dependencies \
    -budget-usd 25 -timeout 3h -report ../artifacts/agent-eval-fast.json
go run ./eval/agent/cmd -suite fr_v1 -agent-suite agent_failures_v1 -tier full \
    -config ../.eval/release-eval-config.json -require-models -require-dependencies \
    -budget-usd 100 -timeout 12h -report ../artifacts/agent-eval-full.json
go run ./eval/agent/cmd -verify-report ../artifacts/agent-eval-full.json \
    -review ../artifacts/agent-review.json -config ../.eval/release-eval-config.json -gate
```

Flags: `-preflight`, `-config`, `-require-models`, `-require-dependencies`, `-suite`,
`-agent-suite`, `-tier`, `-case`, `-budget-usd`, `-timeout`, `-report`, `-verify-report`,
`-review`, `-gate`. Exit status 0 passed/allowed, 1 failed/refused, 2 usage error.
`NOMOS_EVAL_TESTDATA` overrides the fixture root (default `../testdata`).

**Preflight** spends nothing: it compares the environment with the release config (models,
web model unset, provider allowlist, pools, PISTE endpoints), the gateway's model list and
prices with the config's dated tariff snapshot, the fixtures with the manifest, the binaries
(pandoc, pdftotext, pdftoppm, tesseract), the database (extensions, other clients), the blob
store and the connectors. `-require-models` and `-require-dependencies` make those problems
fatal — for preflight and for a run, which then stops before spending anything.

**The release config** (`.eval/release-eval-config.json`) is the non-secret configuration a
report can authorise: models, approved returned-model aliases, approved domains, provider
allowlist, pools, PISTE environment and the dated tariffs. Its canonical hash is part of the
configuration fingerprint.

## Fixtures

`testdata/` stays untracked (the owner's choice). `testdata/manifest.json` pins the sha256 and
size of every file the suites use, the dossier states, the documents the runner generates
itself (the pending client email, the injection attempt, the pièce added at the gate — their
text is pinned, a per-trial nonce in the package properties keeps their bytes unique), the
compétences the cases pick (today the built-in `requete-cph-resiliation-judiciaire`, **drafted
for the owner's review**) and the gold lists. Under `-require-dependencies` a missing or changed fixture fails loudly; without
it the problem is still named in the report, never skipped silently.

## The report

`-report` writes a sealed JSON report (its own sha256 over its content) after every trial, so
an interrupted run leaves an honest partial one. It carries: the commit (and whether the tree
was dirty) and the configuration fingerprint (release config, tool schemas, templates,
verifier version, suites, fixture manifest, evaluation compétence); the corpus and built-in
compétence hashes and the observed prompt hash; exact trial counts and outcomes; coverage and
material-event completeness; legal refusals and pièce warnings; model calls by purpose with
failures, unreconciled calls, unknown dispatches and corrective decisions; input, cached,
reasoning and output tokens; server-tool counts and billed cost; latency and cap/deadline
outcomes; artifact hashes; and per trial the requested → returned models.

Beside it, `<report>.outputs/<trial>/` holds what the lawyer reviews: `answer.md` (the
delivered answer, every published citation — its `[n]`, source type, label, exact identity
(pièce, article version and validity, decision, page), locator, quote and checks — and the
questions asked), the published files, and `bound-output.json`, whose hash is the trial's
`output_sha256` and binds those citations with the text.

## The lawyer's review and the gate

The review file binds a named reviewer's verdicts to output **hashes**:

```json
{
  "version": 1,
  "reviewer": {"name": "…", "role": "avocat", "bar": "Barreau de …"},
  "reviewed_at": "2026-10-20T10:00:00Z",
  "commit": "<report fingerprint.commit>",
  "config_fingerprint": "<report fingerprint.config_fingerprint>",
  "report_sha256": "<report report_sha256>",
  "outputs": [{"trial_id": "fr_v1/c05-s1#1", "output_sha256": "…", "verdict": "approved", "notes": ""}]
}
```

An output that changed after it was read no longer matches its hash and is unreviewed.
`-gate` allows release only for a **full-tier**, sealed, passing report made under
`-require-models -require-dependencies` with no `.env` file read, with no unknown usage or model
issue, for **this commit** (clean tree) and the configuration fingerprint recomputed from this
checkout and `-config`, with every gold list the suites use and every manifest compétence signed
by a lawyer (a manifest `status` starting with `reviewed`, e.g. `reviewed by Me …, 2026-10-20`;
the manifest's hash is in the fingerprint, so a sign-off invalidates earlier reports), with a
review approving every output at its hash. Fast-only success, another
commit's report, a missing or partial review, a timeout or budget stop, unknown cost and
missing dependencies are each refused, with the reason.

## Tests and modes

`NOMOS_LIVE=0 go test -p 1 ./eval/... ./testsupport/... -count=1` runs the harness's own tests
against a disposable Postgres with a scripted fake gateway: oracle pass/fail, budget stop →
`incomplete_budget`, unknown usage blocks, model substitution fails, scripted faults reach the
runtime, review hash mismatch refused, the gate refusing fast-only and other-commit reports.
The modes are shared with the rest of the suite (`api/testsupport/testmode`):
`NOMOS_LIVE=0` excludes every paid or networked suite, `NOMOS_REQUIRE_DEPENDENCIES=1` turns a
missing dependency into a failure, `NOMOS_TEST_NO_DOTENV=1` never reads a `.env` file nor
falls back to the developer database.

## Known limits

- The gold lists, the CPH compétence and the "approved domains" are the evaluation's drafts;
  the owner signs them as the lawyer before a full run can mean anything.
- The Marchand state mixes unreadability with a non-employment domain: it is the only scanned
  fixture.
- Stale publication is exercised by adding a pièce as soon as the final gate starts; when the
  gate reads the scope after the addition, the trial still checks the invariant (the published
  verification is bound to the dossier as it stands) without having forced the refusal path.
- The restart case stops and restarts the runner pools in-process; leases left by interrupted
  steps expire (90 s) before the reaper requeues them.
