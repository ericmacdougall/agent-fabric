# Cost control: state of the art and where Fabric stands
Status line: pinned to commit bc5a6d3; sources accessed 2026-11-07.

## What this component does in Fabric
- The native loop holds the worst-case cost of each request before sending it, and settles the actual cost after. Worst case counts every body byte as an input token at the dearest input price plus `max_tokens` at the output price, with the long-context uplift (internal/agent/prices.go:83-99). `Ledger.Hold` checks every cap and holds all or none (internal/agent/ledger.go:93-123); `Settle` closes the holds with actual cost (ledger.go:137-150).
- Three scopes are held per request: month (keyed by UTC `YYYYMM`), job (per mission) and run (per attempt) (internal/native/native.go:224-229). Ceilings are constants: USD 20 a month, USD 5 a job, USD 1 a live test (native.go:34-36). A grant can lower them, not raise them (native.go:125-128).
- After a crash, `Reap` charges any hold left HELD at its held amount, because the request may have been billed (ledger.go:154-175).
- Provider errors that mean a spend cap are non-retryable and park the mission: 429 without `retry-after`, 400 matching a spend-limit message, 402 (internal/provider/anthropic/client.go:198-203). A `retry-after` beyond the configured maximum also parks (client.go:207).
- Metrics export the cap constants, held and capacity per resource, and month-to-date spend (internal/metrics/metrics.go:223-243).

## State of the art
- Reserve the maximum, reject before the call if over budget, then replace the reservation with actual cost. Reservation is on by default; turning it off lets concurrent requests overshoot. LiteLLM, [users and budgets](https://docs.litellm.ai/docs/proxy/users) (accessed 2026-11-07), H.
- Budget scopes (global, team, member, key, user, customer) with reset windows such as 30d. Same page, H.
- Fail-closed budget enforcement returns 503 when spend cannot be verified; without a database budgets fail open. Batch submissions reserve nothing and zero-priced models skip checks. Same page, H.
- USD and token budgets, weekly or monthly UTC reset, alert thresholds that notify without stopping, and a hard stop at the limit; spend is tracked only for models with known pricing. Portkey, [budget limits](https://portkey.ai/docs/product/ai-gateway/virtual-keys/budget-limits) (accessed 2026-11-07), H.
- Provider-side ceilings: organization monthly caps by tier, a lower self-set limit, per-workspace spend and rate limits, and a 429 with `enforced_spend_limit_reached` and no retry-after when the cap is hit, which SDK retries cannot cure. A self-set limit returns HTTP 400. Anthropic, [rate limits](https://platform.claude.com/docs/en/api/rate-limits) (accessed 2026-11-07), H.
- Client-side caps: `--max-budget-usd` stops a print-mode run at an estimated spend that "can differ from your bill", and `--max-turns` bounds turns. Claude Code, [CLI reference](https://code.claude.com/docs/en/cli-reference) (accessed 2026-11-07), H.
- Circuit breaker for runaway failure with windowed counters, manual override and an event on state change. Azure Architecture Center, [circuit breaker](https://learn.microsoft.com/en-us/azure/architecture/patterns/circuit-breaker) (accessed 2026-11-07), H.
- Rate limits, quotas, timeouts, queue limits and anomaly monitoring as mitigations for unbounded consumption. OWASP, [LLM10:2025](https://genai.owasp.org/llmrisk/llm102025-unbounded-consumption/) (accessed 2026-11-07), H.

## Comparison
| Practice | Fabric today (file:line and test) | Verdict | Why |
|---|---|---|---|
| Hold before send, settle after | ledger.go:93-150; `TestBudgetHeldBeforeSend` (budget_test.go:20) checks a durable hold exists when the request reaches the server; `TestBudgetExhaustedSendsNothing` (budget_test.go:44) | parity | LiteLLM does the same by default. Fabric has no off switch for holding, but it serves one request type, so the comparison is narrow. |
| Cumulative caps across scopes | Month, job, run in one all-or-nothing hold (native.go:224-229); `TestBudgetCumulative` (budget_test.go:57), `TestMonthCeilingStopsLoop` (month_test.go:54) | parity | LiteLLM and Portkey have more scopes (team, key, customer) and configurable windows. Fabric's month window is only the key name. |
| Crash after send charged at worst case | `Reap` (ledger.go:154-175); `TestHoldSurvivesCrash` (budget_test.go:130), `TestTransportAfterWriteCharged` (crash_test.go:188) | parity (peer not documented) | The cited docs do not describe orphaned reservations. Fabric overcounts rather than undercounts, which is the safe side. |
| Missing or incomplete price table fails closed | `TestPriceConfigFailsClosed` (budget_test.go:100): every field required, unknown fields refused, empty table cannot build a loop | parity (peer not documented) | LiteLLM documents that zero-priced models skip checks and Portkey that unknown-price models are untracked. Fabric refuses a missing field; whether an explicit zero price is accepted was not checked. |
| Fixed ceilings that a grant cannot raise | native.go:34-36, 125-128; `TestGrantMonthCeiling` (cap_test.go:21) | different scope | Peers make budgets editable at runtime with an admin API. Fabric trades flexibility for a reviewed constant; there is no per-mission override and no audited change path. |
| Spend-cap errors park instead of retrying | client.go:198-203; `TestRetryMatrix` subtests "429 without retry-after parks at once" and "400 spend limit parks at once" (client_test.go:262, 271) | parity | Matches Anthropic's documented behavior. Any 429 without `retry-after` parks, so an undocumented rate-limit 429 would park too (safe, may be early). The research note listed this as missing; it is present. |
| Reconciliation against provider usage | Not found | behind | Ledger cost is an estimate; no comparison with the provider's usage report and no drift alarm. |
| Provider-side backstop | Not in code; configuration outside the repo | behind | A dedicated provider workspace with its own spend limit is a ceiling that survives a Fabric bug. Not verifiable from the repo. |
| Soft thresholds before the hard stop | Not found; metrics expose spend and caps (metrics.go:230-243) so an external alert could compute it | behind | Portkey alerts at configurable thresholds. |
| Reservation health metrics | Held amount per resource only (metrics.go:225); no reserved/settled ratio or oldest unsettled hold age | behind | Needed to spot leaked holds early. |
| Behavior when the ledger store is down | `Hold` returns the database error and the loop does not send (ledger.go:93-123); no dedicated test found | parity (peer not documented) | LiteLLM offers an explicit fail-closed flag. Fabric appears closed by construction, but unproven by a test. |
| Crash-loop breaker | `RestartCap`; `TestCrashLoopBreaker` (crash_test.go:147) bounds spend by the holds taken | parity | Closest to the breaker pattern; no half-open state. |

## Upgrades, ranked
| rank | upgrade | value | effort | gate or risk addressed | ticket |
|---|---|---|---|---|---|
| 1 | Dedicated provider workspace with a lower spend limit (configuration, documented) | High | Low | Fabric bug or ledger drift overspends | TBD |
| 2 | Daily reconcile of ledger spend against the provider usage report, alert on drift | High | Med | Estimate versus bill gap | TBD |
| 3 | Test that a ledger or database outage sends nothing (failure-first) | Med-High | Low | Unproven fail-closed claim | TBD |
| 4 | Soft-threshold metrics at 50 and 80 percent of job and month caps | Med | Low | Surprise at the hard stop | TBD |
| 5 | Metrics: reserved-to-settled ratio and age of the oldest HELD reservation | Med | Low | Leaked holds, over-reserving | TBD |
| 6 | Audited, expiring cap override through the operator actor and reason | Med | Med | Raising caps today needs a code change | TBD |

## Not verified
- Whether an explicit zero price is rejected by `ParsePrices`; only missing-field and unknown-field refusal was read in the test.
- Behavior of the `claude`-CLI provider path (internal/provider/claude) for spend caps; only the Anthropic API client was checked.
- Whether the month window rolls correctly across a UTC month boundary mid-attempt: the key is computed once per run (native.go:224).
- The USD figures in the Anthropic rate-limits page tiers were not needed and not restated.
