# Evidence

Every directory here is the untouched output of one run, written by `scripts/evidence.sh`. Re-run that script to regenerate all of it.

**What produced it**

- The two discovery runs (01, 06) are real model-driven runs against the live simulator: model `claude-opus-5-5` through the `claude-cli` backend (the Claude Code login; see README "Setup"). Re-running the script produces new runs, not these byte for byte, and rewrites `artifacts/*.json`. The `anthropic` API backend shares the agent loop and is covered by tests, but was not used for these runs.
- The human operator in 06 and 07 was played by a script calling the operator API with `cua operator …` — the same endpoints the HTML console calls. The control transfer, the inputs sent to the live session and the hand-back are real; the person is not.
- The application is the local MeridianCore simulator. All member data is synthetic.
- `../docs/demo.mp4` is a screen recording of the same commands being run end to end (discovery, review, replays, a handoff, the second tenant).

**What is in a run directory**

| File | Content |
|---|---|
| `events.jsonl` | The structured log: one event per line with `ts`, `run`, `seq`, `actor` (`automation`, `human:<name>`, `policy`, `system`), `event`, `step` (when one applies) and `data`. For discovery it includes every model decision with its stated reason. |
| `discovery.json` / `result.json` | The run's result as persisted. Sensitive inputs and outputs appear as `[input:name]` / `[output:name]` here; the caller received the real values on stdout. |
| `artifact.json` | (discovery) The capability that was recorded; identical to the file in `artifacts/`. |
| `turn-NN.png` | (discovery) A redacted screenshot of the screen at each turn. The model itself receives the text rendering of the same screen (see `agent.decision` events), not the image. |
| `NN-failure.*`, `NN-outcome-*.*`, `NN-handoff-*.*` | A redacted screenshot and observation snapshot taken at a failure, a business outcome, or an intervention request. |

## Runs

| # | Run | What it shows | Result |
|---|---|---|---|
| 01 | `01-discovery-savings-balance` | The goal was given as the sentence "Look up member 10042 and read their current savings balance" (the number became the `member_id` input). The model signs on, opens Member Inquiry (a clickable table cell in another frame), searches, and designates the savings balance by row and column. 8 turns, 7 steps recorded, 36 s. It never sees the password (it types `{{secrets.operator_password}}`) or the balance (masked as `[AMOUNT]`). | `success`, artifact written |
| 02 | `02-replay-success` | The artifact from 01 replayed for a *different* member (10058). No model. 0.9 s. | `success` |
| 03 | `03-replay-business-outcome` | Member 99999 does not exist. The `record_not_found` state is recognised while waiting for the detail screen. | `business_outcome: record_not_found` |
| 04 | `04-replay-recovered` | Three injected conditions in one run: an interstitial notice (dismissed), the session expiring as the search is submitted (flow restarted from a fresh session), and a page delayed by 3.5 s (waited for; `waited_ms` in the result). | `success`, 3 recoveries, 2 attempts |
| 05 | `05-replay-hard-failure` | Member 10063 exists but has no Regular Savings share, and the artifact declares no outcome for that. The extract step's row anchor is missing; nothing is guessed. | `failed: target_not_found` at the extract step, with expected, observed, screenshot and observation |
| 06 | `06-discovery-open-sub-account` | A write flow ending in a committing step. At "Confirm" discovery stops and raises an approval request; the operator approves; the model then answers the native confirm dialog, and the committing POST is released at the network guard (`policy.irreversible_request`). 17 turns, 15 steps, 83 s. | `success`, artifact written, 1 handoff |
| 07 | `07-replay-human-handoff` | A $12,000 deposit makes the application demand a supervisor override code that automation does not hold. Control goes `automation → pending_human → human` and back (five transfers are logged over the two handoffs); the operator clicks the field, types the code (logged as `text_len: 8`, never the text), clicks Authorize, and hands back with `resume`; replay verifies the screen it was waiting for and continues; the committing step then needs and gets approval. | `success`, 2 handoffs, 3 recorded human actions |
| 08 | `08-replay-second-tenant` | The artifact recorded on Pinecrest replayed on Lakeshore — same product, three labels worded differently — through the tenant binding's label overrides. | `success`, no drift |
| 09 | `09-replay-tenant-drift` | The same, with a binding that has no overrides. The primary locator misses, a positional fallback is used and reported under `drift`, and the step's screen checkpoint stops the run. | `failed: checkpoint_failed`, 1 drift entry |
| 10 | `10-replay-validation-outcome` | A business outcome on the write path: the deposit is below the product minimum, the application says so, nothing is committed and no human is asked. | `business_outcome: validation_error` |

## Reading a log

```bash
# Every model decision in the first discovery run, with its reason
jq -r 'select(.event=="agent.decision") | "\(.data.turn). \(.data.action) \(.data.element // "") — \(.data.reason)"' \
  evidence/01-discovery-savings-balance/events.jsonl

# The control transfers and human actions in the handoff run
jq -c 'select(.event|test("control|human|handoff"))' evidence/07-replay-human-handoff/events.jsonl
```
