# Relevance evaluation harness

**What it answers:** does Nomos retrieval actually find the right article, and
which of the four retrieval strategies is worth paying for?

`docs/PLAN.md` §6.2 fixes a design — vector arm + French full-text arm, fused by
reciprocal rank fusion, then reranked by a cross-encoder — and §6.3 says out loud
what that design does *not* establish. Neither statement is a measurement.
Without a scored query set, "hybrid beats vector" is a claim, "the reranker earns
its money" is a hope, and §15's open decision 3 ("is open-sources-only retrieval
useful enough?") is unanswerable.

This package is the measurement. It scores **the same code path the product
uses** — `services.Retrieve`, the single entry point behind `POST /api/search` —
because a harness that scores a different path scores nothing.

---

## Running it

```bash
# CI-friendly. Offline metric + query-set tests always run; the live tests SKIP
# with a printed reason when the corpus or the API key is absent.
cd api && GOTOOLCHAIN=auto go test ./eval/... -v

# The human scoreboard: all four strategies over the same query set.
cd api && GOTOOLCHAIN=auto go run ./eval/cmd -strategy all

# Narrow it down while iterating.
cd api && GOTOOLCHAIN=auto go run ./eval/cmd -strategy rrf_rerank -category temporal
cd api && GOTOOLCHAIN=auto go run ./eval/cmd -case voc-001
```

Preconditions, and what happens when they are missing:

| Missing | Behaviour |
| --- | --- |
| `DATABASE_URL` | test skips with "no score produced"; CLI exits 2 with instructions |
| `OPENROUTER_API_KEY` | same |
| corpus not loaded (`legal_chunks` empty for the source) | same, naming the corpus sync to run |

It never fails silently and it never prints a score it did not measure.

The corpus is the committed offline fixture (`api/fixtures/code_du_travail.json`,
282 articles of the Code du travail covering the individual employment dispute,
Licence Ouverte 2.0). Load it with a corpus sync of `source=fixtures`. The
harness pins `legal_documents.source = 'fixtures'` so that other packages' seeded
rows in the shared dev database cannot contaminate the score.

---

## The query set

106 cases in `testcases/*.json`, every one carrying an `id`, a `category`, a
French query written the way a lawyer types it, the gold articles, and a `note`
explaining why the case is worth having. `TestSuiteIsValid` checks — offline, no
database — that every gold, forbidden and near-miss article actually exists in
the fixture corpus: a typo there silently caps recall for every strategy at once
and looks exactly like a retrieval regression.

| File | n | What it stresses |
| --- | --- | --- |
| `01_lookups.json` | 15 | statutory vocabulary, the floor |
| `02_rupture.json` | 12 | dismissal — the practice area slice 2 depends on |
| `03_vocabulary_mismatch.json` | 10 | the lawyer's words are not the statute's words |
| `04_contrat.json` | 10 | formation of the contract, CDD regime |
| `05_procedure.json` | 12 | prud'homal procedure, short formulaic R articles |
| `06_multi_hop.json` | 7 | no single article answers |
| `07_temporal.json` | 10 | the answer depends on the as-of date |
| `08_negative_controls.json` | 12 | the corpus cannot answer — abstain |
| `09_near_miss.json` | 10 | two similar articles, one right |
| `10_variants.json` | 8 | accent/casing twins must score identically |

Four kinds of expectation, deliberately kept separate because they fail for
different reasons and must never be traded against each other:

- **`gold`** — must come back. Graded: **2** = the article that *answers*,
  **1** = an article a lawyer would cite alongside it.
- **`forbidden`** — must not come back at any rank. Used by the temporal cases:
  an article not in force at the date of the facts is *wrong law*, not a bad
  rank (§6.4).
- **`near_miss`** — may come back, but must not outrank every primary gold
  article. This is the failure recall cannot see: both articles are in the top
  10, and the lawyer reading top-down gets the wrong rule.
- **negative controls** (no gold) — nothing may cross the relevance threshold.

---

## The metrics

Implemented from scratch in `metrics.go` and unit-tested against hand-computed
values in `relevance_test.go`, because a metric only ever exercised by the
pipeline it grades is not a measurement either.

The unit of relevance is the **article**, not the chunk: several chunks of one
article count once, otherwise a system that returns one article three ways
scores precision@3 = 1.0 while showing the lawyer one article.

| Metric | Definition | Why it is here |
| --- | --- | --- |
| **recall@k**, k ∈ {1,5,10,20} | share of gold articles in the top k | the one that matters most: a lawyer can find an article that is on the page and cannot use one that never came back |
| **MRR** | 1/rank of the first relevant article | how far down the page before the first useful thing |
| **nDCG@10** | Σ (2^grade − 1)/log₂(rank+1), over the ideal ordering | the only metric that uses the grades, so the only one that distinguishes "the answering article is 1st" from "it is 9th behind eight supporting ones". **This is the regression gate.** |
| **precision@5** | relevant articles in the top 5, over 5 | the five slots the UI actually shows. Denominator is k, not the number of results returned |
| **abstention rate** | share of negative controls where *no* hit reaches the threshold | catches the failure every other metric rewards: a system that always confidently returns ten articles |
| **temporal leaks** | cases where a forbidden (not-in-force) article was returned | correctness, not ranking |
| **near-miss inversions** | cases where an impostor outranked every primary gold article | correctness, not ranking |
| **variant agreement** | slot-for-slot top-k agreement between an accented query and its unaccented/upper-cased twin | 1.000 or it is a normalisation bug |

**Cost and latency** are reported per strategy. Cost is *modelled* from published
list prices (openai/text-embedding-3-small $0.02/M tokens; cohere/rerank-4-fast
$0.002/search unit, one unit being one query with up to 100 documents of ≤500 tokens) — the audited
figures live in `llm_calls` (§4.6). `TestCostModel` pins the
arithmetic so the column is reproducible.

---

## Adding a case

1. Put it in the file matching what it stresses, or start a new one — every
   `testcases/*.json` file is picked up automatically.
2. Write the `note`. `LoadSuite` rejects a case without one: a case nobody can
   explain is a case nobody can maintain, and gold sets rot faster than code.
3. Gold articles are bare Code du travail numbers as the fixture writes them —
   `L1234-9`, `R1455-7`, `L1453-1A` — never citation labels. The label format is
   presentation and the gold set must survive it being reworded.
4. Run `go test ./eval/... -run TestSuiteIsValid`. It will tell you if an article
   does not exist in the corpus, if a grade is out of range, if a negative
   control has gold, or if a variant's gold differs from its reference.
5. If the case needs an article the fixture does not have, **extend the fixture**
   with real Licence Ouverte text rather than weakening the case.


---

## The measured baseline

> **These numbers predate the current model pair.** They were measured with
> `baai/bge-m3` (1024-dim) and `cohere/rerank-v3.5`. The app now embeds with
> `openai/text-embedding-3-small` (1536-dim) and reranks with
> `cohere/rerank-4-fast`. The re-measured headline is at the end of this section;
> the per-category tables and the three weaknesses below have NOT been re-run and
> are kept as the analysis that motivated them.

Produced by `go run ./eval/cmd -strategy all` on **2026-08-27**, against the
committed fixture corpus (`source=fixtures`, 282 articles / 284 embedded chunks,
`legal_versions.state='vigueur'`), embeddings `baai/bge-m3`, rerank
`cohere/rerank-v3.5`, top-k 10. 106 cases, of which 88 are graded (the other 18
are negative controls and the leak-only halves of the temporal pairs).

```
strategy         R@1     R@5    R@10    R@20     MRR  nDCG@10     P@5       p50       p95    USD/1k q
----------------------------------------------------------------------------------------------------
vector         0.375   0.809   0.896   0.896   0.911    0.835   0.418     544ms    1.716s    0.000192
fts            0.090   0.346   0.499   0.499   0.367    0.380   0.177       3ms       7ms    0.000000
rrf            0.316   0.707   0.828   0.828   0.811    0.761   0.361     541ms    1.076s    0.000192
rrf_rerank     0.381   0.812   0.894   0.894   0.918    0.856   0.416     804ms    1.359s    1.000193

PER CATEGORY — nDCG@10 / recall@10 (n = graded queries)
category                n              vector                 fts                 rrf          rrf_rerank
---------------------------------------------------------------------------------------------------------
contrat                15     0.855 /   0.889     0.233 /   0.367     0.790 /   0.844     0.893 /   0.911
droit_du_travail       27     0.857 /   0.926     0.362 /   0.444     0.797 /   0.861     0.860 /   0.901
procedure              18     0.784 /   0.852     0.418 /   0.620     0.720 /   0.824     0.794 /   0.852
rupture                24     0.815 /   0.903     0.429 /   0.545     0.725 /   0.795     0.858 /   0.910
temporal                4     0.957 /   0.875     0.578 /   0.542     0.807 /   0.750     0.957 /   0.875

BEHAVIOURAL CHECKS
strategy          abstention (neg ctrl)         temporal leaks   near-miss inversions      variant agree
---------------------------------------------------------------------------------------------------------
vector               n/a (uncalibrated)             0/6                    3/10                   0.350
fts                  n/a (uncalibrated)             0/6                    2/10                   1.000
rrf                  n/a (uncalibrated)             0/6                    4/10                   0.350
rrf_rerank                  83% (10/12)             0/6                    2/10                   0.575
```

### Re-measured 2026-08-28 — `openai/text-embedding-3-small` + `cohere/rerank-4-fast`

Same suite, same corpus, same top-k. Relevance is unchanged to within noise; what
moved is the abstention threshold and the cost.

```
strategy         R@1     R@5    R@10    R@20     MRR  nDCG@10     P@5       p50    USD/1k q
--------------------------------------------------------------------------------------------
vector         0.364   0.799   0.894   0.894   0.882    0.819   0.411     435ms    0.000385
fts            0.090   0.346   0.499   0.499   0.367    0.380   0.177       4ms    0.000000
rrf            0.297   0.722   0.821   0.821   0.785    0.740   0.366     439ms    0.000385
rrf_rerank     0.385   0.776   0.902   0.902   0.918    0.857   0.395     744ms    2.000385

BEHAVIOURAL CHECKS
strategy          abstention (neg ctrl)         temporal leaks   near-miss inversions      variant agree
--------------------------------------------------------------------------------------------------------
vector               n/a (uncalibrated)             0/6                    3/10                   0.375
fts                  n/a (uncalibrated)             0/6                    4/10                   1.000
rrf                  n/a (uncalibrated)             0/6                    2/10                   0.450
rrf_rerank                   75% (9/12)             0/6                    1/10                   0.500
```

**The abstention threshold had to be re-derived, and that is the important part
of this switch.** `rerank-4-fast` scores every document higher than `v3.5` did, so
the old 0.30 cut landed inside the negative-control distribution and abstention
collapsed from 0.75 to 0.42 — the system became *more* confident about questions
the corpus cannot answer. Measured over the suite, the two populations still
separate cleanly (negative controls: min 0.1215, p50 0.3087, max 0.8896; graded:
min 0.6312, p50 0.9160), so the threshold is now 0.60 — the highest value that
suppresses no graded query. It restores abstention to 0.75 at zero cost to recall.
**Re-measure it whenever the reranker changes**; `abstentionThreshold` in
`eval/eval.go` carries the distribution it was derived from.

Cost per 1k queries doubled (1.00 → 2.00) because `rerank-4-fast` bills $0.002 per
search unit against v3.5's $0.001.

Recorded as the regression floor in `relevance_test.go`, with a margin under
each measurement for the cross-encoder's run-to-run variation:

| Constant | Floor | Measured |
| --- | --- | --- |
| `BaselineNDCG10` | 0.840 | 0.857 |
| `BaselineRecall10` | 0.875 | 0.894 |
| `BaselineAbstention` | 0.75 | 0.833 |
| `BaselineVariantAgreement` | 0.50 | 0.575 |

**Does the reranker earn its money?** Over RRF: **+0.048 nDCG@10**, +0.060 MRR,
+250 ms p50, and **5 200× the cost** ($1.00 vs $0.0002 per 1 000 queries). Over
the vector arm alone — the honest comparison, because RRF is currently below it —
it buys +0.022 nDCG@10 and +0.012 MRR for the same money. On ranking alone that
is a thin case at this corpus size. What actually justifies it is the column
next to it: **`rrf_rerank` is the only strategy that can abstain at all**, because
it is the only one with a calibrated 0–1 relevance score. In a legal product,
"the corpus cannot answer this" is worth more than 0.02 nDCG.

---

## Where retrieval is weak

The useful half of the report. Every item below is a measurement, not a worry.

**1. The shipped default configuration returned zero hits against this corpus.**
**FIXED — this is what the harness was for.** Retrieval filtered
`legal_versions.state = 'active'` while the loader wrote LEGI's own vocabulary,
`'vigueur'`. Nothing reconciled them, so `POST /api/search` found nothing at all
against a fully loaded corpus — a total functional break that no unit test
noticed, because every unit test seeded its own fixtures with the reader's
spelling. Both sides now go through `types.NormalizeLegalVersionState` and the
canonical vocabulary is Légifrance's (`vigueur` / `abroge` / `modifie` / …).
`Preflight` keeps the tripwire: it compares what the corpus actually contains
against what retrieval actually asks for — hardcoding neither — and logs
`DEFECT: …` on every run if they ever drift apart again.

**2. The lexical arm was dead. It is now alive — and that made the honest
answer worse, not better.**

*The defect (fixed).* `fts` scored nDCG@10 **0.167**, recall@10 **0.125**, and
exactly **0.000** in the `contrat` category. The cause was not stemming or
`unaccent`: `websearch_to_tsquery` **ANDs every term**, so one out-of-vocabulary
token emptied the whole arm.

```
websearch_to_tsquery('french', unaccent('durée de la période d''essai en CDI'))
  → 'dure' & 'period' & 'essai' & 'cdi'      → 0 rows   (0 chunks matched)
  (drop the one token "CDI"  → 'period' & 'essai'  → 5 rows)
```

A lawyer's natural-language query runs 10–20 tokens, so the conjunction almost
never held. The arm now builds an **OR of its own stemmed lexemes** with a
`ts_rank_cd` floor. Same query, same corpus: **0 rows → 223 rows**, and `fts`
went **0.167 → 0.380** nDCG@10, recall@10 **0.125 → 0.499**, `contrat`
**0.000 → 0.233**.

*What that revealed.* With a real lexical arm, equal-weight RRF **fell** from
0.809 to 0.673 — because the old 0.809 was never fusion, it was `vector` plus
noise from a near-empty arm. Weighting the arms (dense 1.0, lexical 0.4)
recovers it to **0.761**, but that is still below `vector` alone (**0.835**).

**So the honest statement is: on this corpus, hybrid does not beat dense.** The
shipped `rrf_rerank` (0.856) is the best configuration because the reranker
repairs the fusion, not because the fusion helps. The lexical arm earns its place
on *complementarity* — it rescues queries the dense arm misses outright, and it
is the only arm that is fully accent-invariant (finding 3) — not on average
score. A 282-article corpus with a strong multilingual embedder is close to the
worst case for lexical retrieval; expect the ratio to move at Légifrance scale,
where exact article-number and citation matching is what the dense arm is worst
at. Re-measure before changing the weights.

**3. Accent and casing invariance is only half true.** §6.2 says `unaccent` is
applied to the query exactly as it was applied to the text at write time. That is
true of the lexical arm — it scores **1.000** slot-for-slot agreement — and false
of the dense arm, which scores **0.350**: `bge-m3` embeds "congés payés" and
"conges payes" as different points. `rrf_rerank` lands at 0.575. A lawyer typing
without accents gets a different ranking, which no user would predict.

The 2026-08-28 re-measurement confirms this is a property of dense retrieval
rather than of one model: with `text-embedding-3-small` the dense arm scores
**0.375** and `rrf_rerank` **0.500**. Changing the embedding model did not fix it.

The obvious fix — "unaccent the query before embedding it" — is **wrong on its
own**, and worth spelling out so nobody ships it: the corpus was embedded from
*accented* text, so unaccenting only the query moves it away from the stored
points rather than towards them. Making the two sides agree means normalising
*both*, i.e. re-embedding the corpus from unaccented text, which throws away
accent information that is meaningful in French (`congé` vs `conge`). That is a
corpus-wide decision with a real downside, so it is recorded here rather than
taken quietly. Until it is taken, the lexical arm is what carries accent-
insensitive queries — which is a concrete reason to keep it in the fusion.

**4. Abstention fails on exactly the queries that matter.** 10 of 12 negative
controls are correctly refused. The two that are not are the two *near-domain*
ones — employment-law questions whose articles are outside the ingested subset:

```
neg-008  durée de la période d'essai d'un CDI …          → L1242-10 at 0.703
neg-009  contingent annuel d'heures supplémentaires …    → L3123-2  at 0.704
```

The cross-encoder scores a topically adjacent but legally wrong article at 0.70 —
the same band it gives correct answers. Raising the threshold to 0.75 would fix
these two and start suppressing right answers; the score is topical, not
juridical. **A confident wrong article on a question the corpus cannot answer is
the single most dangerous output this system produces**, and no metric other than
this column sees it. The other three strategies are marked `n/a`, not 100%: an RRF
score is a sum of `1/(60+rank)` and its top hit scores ~0.033 whether the corpus
answers or not, so they *structurally cannot abstain*.

**5. Near-miss inversions survive the reranker.** 2 of 10 for the best strategy,
4 of 10 for `rrf`. `nm-005` (CDD rupture anticipée: `L1243-4` outranks the right
article) and `nm-010` (mise à la retraite: `L1237-9` outranks `L1237-7`) fail in
every configuration. These articles are lexically near-identical and differ on one
legal condition; neither the bi-encoder nor the cross-encoder resolves that, and
recall@10 cannot see it because both articles are on the page. This is the failure
mode a lawyer reading top-down actually hits.

**6. Procedure is the weakest category** (nDCG@10 0.794, recall@10 0.852). The R
articles of the prud'homal procedure are short and formulaic; several worst-cases
are procedural (`voc-010`, `voc-006`, `pro-011`, `pro-007`). Colloquial procedural
phrasing — "attaquer aux prud'hommes", "condamner tout de suite" for *référé* —
is where vocabulary mismatch bites hardest, because the statutory word (`référé`,
`prescription`) never appears in the query at all.

**7. The temporal cases prove exclusion, never substitution.** 0 leaks out of 6 —
but the fixture corpus contains **no article with more than one version**. The
harness verifies that an article not yet in force is unreachable at an earlier
as-of date; it has never verified that a 2016 query returns the *2016 wording* of
an article that has since changed. That requires superseded versions in
`legal_versions`, and until the Légifrance connector supplies them, `0/6` is much
weaker evidence than it looks.

**8. The corpus is 282 articles of employment law.** These absolute numbers will
fall when the corpus reaches Légifrance scale: recall@10 of 0.894 over 284 chunks
is not a prediction of recall over two million. Small corpora also flatter
negative controls (less to be wrong with) and flatter the dense arm (fewer
near-duplicates). Read this scoreboard as **a comparison between strategies on a
fixed set**, which is what it is designed to be, and re-baseline it the moment the
real corpus lands. **P@5 ≈ 0.42 is not a defect** either: most cases have one to
three gold articles, so the denominator of 5 caps it — read recall and nDCG.

**Answer to §15, open decision 3, as far as this corpus can support one:** on a
curated Licence Ouverte subset, open-source-only retrieval reaches the right
article in the top 10 for ~89% of realistic queries and puts it first for ~38%.
That is usable *as a drafting aid with mandatory citation verification*, and it is
not usable as an answer engine — the two near-domain negative-control failures and
the two persistent near-miss inversions are exactly the cases where an unverified
answer would be confidently wrong.
