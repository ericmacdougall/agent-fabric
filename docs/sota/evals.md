# Evals and promotion gates: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- Control-plane correctness is tested with unit tests (80 `_test.go` files), a seeded random-trace property test that checks replay equals live state and prints its seed (`internal/reducer/reducer_test.go:343-413`), and database-backed integration tests.
- 343 must-fail mutants are applied to a scratch copy; a mutant counts as killed only if the run fails and the named test is among the failures; a survivor or a build failure fails the gate (`tools/mutants/mutants.json`, `tools/mutants/run.py:1-12`). The runner first requires the unmutated suites to pass (`run.py:55-62`). Separate mutant sets exist for the agent loop, provider and tools (`tools/native-mutants/mut-agent.json`, `mut-provider.json`, `mut-tools.json`).
- Host sandbox profiles have negative controls that must fail when a protection is removed (`internal/host/host_test.go:43`, `TestNegativeControls`); provider stream fixtures have failing controls (`internal/provider/anthropic/fixtures_test.go:150`).
- `TestNoModelLiteralsInSource` (`internal/provider/claude/claude_test.go:177`) forbids model names in source outside the tier table.
- Nothing measures model or agent quality: no task suite, no repeated trials, no confidence intervals, no baseline comparison. A search for Wilson, McNemar, pass@k and pass^k in Go and Python files found no hits.
- Known state at this commit, as given in the task brief and not re-run for this note: the mutant gate cannot run because `TestNoModelLiteralsInSource` fails on the unmutated tree, so the baseline check stops it; metrics and TUI golden files are stale (the metrics golden shows a `usd_api` capacity of 100000000 micros at `internal/metrics/testdata/golden.prom:45`, while the native month cap in code is USD 20, `internal/native/native.go:34`).

## State of the art
- Anthropic's agent-eval guide: code-based, model-based and human graders; grade what the agent produced rather than its path; run each trial from a clean environment; capability evals start at a low pass rate and graduate into regression evals that should sit near 100%; pass@k (at least one success in k) differs from pass^k (all k succeed); read transcripts to check graders. [Demystifying evals](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents) (accessed 2026-11-07), H.
- tau-bench introduced pass^k; strong function-calling agents solved fewer than half the tasks and retail pass^8 was below 25% (abstract). [tau-bench](https://arxiv.org/abs/2406.12045) (accessed 2026-11-07), H for the abstract.
- Miller, "Adding Error Bars to Evals": treat questions as a sample, analyse paired differences between models, use clustered errors for correlated questions, plan sample size by power. Only the abstract was re-fetched in this pass; details such as clustered errors being about 3 times the naive error came from an earlier read of the HTML version. [arXiv 2411.00640](https://arxiv.org/abs/2411.00640) (accessed 2026-11-07), M.
- Binomial intervals: Wilson is preferred over the normal approximation at small n; with zero failures in n trials the 95% upper bound on failure rate is about 3/n. McNemar's test compares paired binary outcomes using discordant pairs, with an exact test when they are few. [Binomial proportion CI](https://en.wikipedia.org/wiki/Binomial_proportion_confidence_interval), [McNemar](https://en.wikipedia.org/wiki/McNemar%27s_test) (accessed 2026-11-07), H as encyclopedic sources.
- Grader integrity: METR reported frontier models overwriting timers, patching evaluators and searching for leaked answers (about 30% of o3 runs on RE-Bench); "do not cheat" instructions helped little; the advice is to patch the scoring setup. [METR](https://metr.org/blog/2025-06-05-recent-reward-hacking/) (accessed 2026-11-07), H.
- Benchmark validity: OpenAI stopped evaluating SWE-bench Verified over flawed tests and contamination; read through a secondary summary because the primary page returned 403. [The Decoder](https://the-decoder.com/openai-wants-to-retire-the-ai-coding-benchmark-that-everyone-has-been-competing-on/) (accessed 2026-11-07), M.
- Silent production degradation: Anthropic's September 2025 postmortem found existing evals did not catch three infrastructure bugs and moved to continuous quality evals on production. [Postmortem](https://www.anthropic.com/engineering/a-postmortem-of-three-recent-issues) (accessed 2026-11-07), H.
- NIST AI RMF Playbook MEASURE: document test sets and metrics for repeatability, compare production to pre-deployment results to detect drift, report variance or confidence intervals, use independent assessors. [NIST](https://airc.nist.gov/airmf-resources/playbook/measure/) (accessed 2026-11-07), M (part of the page read).

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Deterministic regression tests with failing controls | Mutants and negative controls above; `TestNegativeControls` (`host_test.go:43`), `TestSSEFixtureFailingControls` (`fixtures_test.go:150`) | parity | The guide asks for code graders that cannot pass vacuously. This is a control-plane suite, not an agent-quality suite, so it answers a different question than the peers' evals. |
| Mutation-killed tests | 343 mutants, kill requires the named test to fail (`run.py:1-12`) | different scope | None of the fetched eval sources describes mutation testing of the harness. The gate is currently not runnable (baseline fails), so today it proves nothing. |
| Seeded property tests | `TestTraces` prints seed and checks every refusal and event kind was exercised (`reducer_test.go:343-413`) | parity (peer not documented) | Fits the seed-in-output rule; peers' pages do not cover control-plane property tests. |
| Task-success evals on agent behaviour | Not found | behind | Anthropic, tau-bench and OpenAI all start from realistic task sets with outcome graders. Nothing gates a model, prompt or tool-schema change on task success. |
| Repeated trials (pass^k) and clean-environment trials | Not found | behind | Anthropic and tau-bench show consistency falls with k; Fabric has no measure of it. |
| Intervals and paired comparison | Not found | behind | Without Wilson or exact intervals a small suite's pass rate cannot support a promotion decision. |
| Grader integrity and hidden graders | Not found for model-driven runs; tests live in the same tree the agent edits | behind | METR shows models probe graders. A guard against editing tests is not in the eval path. |
| Held-out, private task set with provenance | Not found | behind | Public benchmark validity is contested; Fabric's own jobs are the natural source. |
| Drift and canary evals | Not found (whether model id and prompt hash are stored per attempt was not checked) | behind | The September 2025 postmortem and NIST MEASURE 2.4 call for production monitoring. |
| Test hygiene on the main branch | Baseline fails (`TestNoModelLiteralsInSource`), stale goldens (task brief; `golden.prom:45`) | behind | A gate that cannot run, and goldens that disagree with code, remove the repeatability the NIST playbook asks for. |

No `ahead` verdicts.

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket: TBD |
|---|---|---|---|---|---|
| 1 | Make the baseline green: fix the model-literal test failure so the mutant gate runs, refresh the metrics and TUI goldens from code, and add a CI step that fails on a red baseline | Restores the only existing quality gate | S | Unrunnable mutant gate | goodqa#15136 |
| 2 | Golden task suite v0: 30 to 60 anonymised real jobs with hidden deterministic graders (tests pass, diff inside allowed paths, no effect outside the broker, spend under cap), tagged capability or regression, with provenance and date | First measure of agent quality | L | No model-quality gate | TBD |
| 3 | Runner with k trials per task from a clean sandbox, storing config hash, model id, prompt hash, tool schema hash, turns, cost and grader result per trial | Flakiness and cost per task become data | M | pass^k, drift | TBD |
| 4 | Promotion rule fixed before a run: Wilson lower bound on regression tasks above a floor, exact paired test on discordant tasks, cost and turn medians within a bound; state the minimum detectable effect for the suite size | Defensible model and prompt changes | M | Silent regression | TBD |
| 5 | Safety sub-suite (injected instructions, test-edit attempts, scope escapes, stuck loops) reported as zero failures in N, with the rule-of-three bound | Bounded safety claim | M | Excessive agency | TBD |
| 6 | Hourly canary probe set with deterministic graders and an alert when the Wilson lower bound drops | Detects provider regressions | M | Drift | TBD |
| 7 | Transcript review of failures and a sample of passes at each promotion; a model judge only for rubric tasks, calibrated on human labels | Grader trust | S | Reward hacking | TBD |

## Not verified
- The mutant gate and the golden mismatch were not re-run for this note; both come from the task brief, with only the `golden.prom` line read directly.
- Whether model id and prompt hash are stored per attempt; not checked in this pass.
- OpenAI's SWE-bench post (403) and later METR figures were not read from primary pages. Later claims about a retraction of a benchmark recommendation were seen only in a search snippet and are not used.
- Miller's clustered-error figure was not re-confirmed in this pass.
