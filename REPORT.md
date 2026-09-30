# Design write-up

Setup, commands and diagrams are in [README.md](README.md); recorded runs are in [evidence/](evidence/).

## 1. Architecture

One Go process and one live session per run, with two paths over it. **Discovery** (`internal/agent`) is an observe → decide → act loop in which a model chooses each action. **Replay** (`internal/replay`) is the same loop with the artifact choosing; no model is on that path. Both use the same collaborators — `surface` (perceive, act), `locate` (find a control), `policy` (may I?), `control` (who is driving?), `evidence` behind `redact` — so they perceive, guard and log identically.

| Decision | Why | Cost |
|---|---|---|
| Perceive through the **accessibility tree**; act with **synthesized mouse and keyboard at coordinates**; no CSS/XPath selectors | The target has no clean DOM. Role + name + rectangle is what a frameset, a table-layout page and a desktop accessibility API all expose, so locators and replay are surface-independent. | A protocol call per element for its rectangle; a native `<select>` popup cannot be clicked, so that action sets the control directly. |
| One schema-constrained JSON action per turn, not a screenshot-and-pixels computer-use tool; **stateless turns** (goal, one line per past step, current screen) | Replay needs *which control*, not *which pixel*. Text observations are cheap, redactable before the model sees them, bounded in size, and reproducible from the log. | No visual reasoning (a canvas-only surface needs an OCR `Observe`, §4); the model cannot re-read an earlier screen. |
| **The model proposes, the harness disposes** | The artifact must be built from verified facts: the harness derives locators, proves each resolves back to the chosen element, classifies risk and records the screen change. The model never picks a locator, sees a secret, or declares a step safe. | The model cannot express a locator the harness cannot derive. |
| Claude Opus 5.5 with structured outputs, behind a two-backend `llm.Client` (Messages API, or the `claude` CLI as a bare model endpoint) | One UI decision per call is routine judgment work; Opus is reliable at it and the schema guarantees a parseable action. The backends share the loop and prompt verbatim. | The recorded evidence came through the CLI login (no API key on the build machine); the API backend is covered by tests only. |
| Single process, synchronous, files on disk; Go + `chromedp` | Nothing here needs a queue or database; services would attach at the seams (surface, escalator, secret reference). One static binary, typed contracts, direct access to the browser's accessibility and network domains. | One run at a time; a smaller ecosystem than Playwright. |

## 2. Artifact schema

An artifact is a **capability**: a contract a calling agent can rely on, plus the flow that fulfils it. Abridged (full files in `artifacts/`; `cua describe` renders one for review):

```jsonc
{
  "schema_version": "1.0",                          // unknown versions are refused
  "id": "member.get_savings_balance", "version": "1.0.0",
  "app": { "profile": "meridian-core", "profile_version": "1", "surface": "web/accessibility-tree" },
  "inputs":   [{ "name": "member_id", "type": "string", "pattern": "^[0-9]{1,10}$" }],
  "secrets":  ["operator_id", "operator_password"],  // names only
  "outputs":  [{ "name": "savings_balance", "type": "money", "sensitive": true }],
  "outcomes": [{ "code": "record_not_found" }, { "code": "validation_error" }, { "code": "permission_denied" }],
  "entry": "/",
  "steps": [
    { "id": "s5", "action": "type", "value": "{{inputs.member_id}}", "risk": "reversible",
      "intent": "Enter the member number to look up the member.",
      "target": { "frame": "main", "locators": [
        { "strategy": "label", "role": "textbox", "label": "Member Number:", "side": "left" },
        { "strategy": "ordinal", "role": "textbox", "index": 1 } ] } },
    { "id": "s6", "action": "click", "target": { "frame": "main", "locators": [ /* button named "Search" */ ] },
      "expect": { "frame": "main", "path": "/member", "title": "Member Detail" } },
    { "id": "s7", "action": "extract", "output": "savings_balance", "risk": "read",
      "target": { "frame": "main", "locators": [
        { "strategy": "grid", "role": "text", "row": "Regular Savings", "column": "Current Balance" } ] } }
  ],
  "success":  { "all": [ { "text": "Member Detail", "frame": "main" },      // the goal screen ...
                          { "text": "{{inputs.member_id}}", "frame": "main" } ] }, // ... for the requested record
  "risk": "reversible",
  "approval": { "status": "draft" },                 // approved => digest of the content reviewed
  "provenance": { "goal": "…{{inputs.member_id}}…", "model": "claude-opus-5-5", "run_id": "…" }
}
```

- **Contract first.** Inputs and outputs are given with the goal (as flags beside a plain-language sentence, or in a request file); outcome codes are copied from the product profile's business states when the artifact is recorded. All are typed, and export as a tool definition (`cua capabilities`). An outcome the artifact does not declare is a *failure*, so callers only see codes the contract lists.
- **Locators are an ordered list of strategies, most robust first**, with a recorded rationale: `name` (role + accessible name), `label` (the text beside an unlabelled field), `grid` (row anchor × column header, for tables), `ordinal` (position; last resort). Each must match **exactly one** element or it is a miss. Values are located only by what anchors them, never by their own text or position.
- **Everything that varied is a template** (`{{inputs.x}}`, `{{secrets.y}}`) in typed values, locator text and expected paths; an artifact holding a concrete input or secret value is refused.
- **`expect` and `success` are checkpoints.** `expect` is recorded by the harness from the observed screen change; `success` is the static proof text the model proposes (accepted only if visible at that moment) plus every non-sensitive string input the harness finds on that screen, so a replay proves it reached the goal *for the requested record*.
- **`risk` per step; `approval` carries a content digest**, so editing an approved artifact voids the approval.
- **Deliberately absent:** the model transcript, tenant URLs, credentials, recorded data, and error-state handling (it belongs to the product; §3).

## 3. Determinism & error handling

**Determinism.** Replay makes no choices: step order is fixed, a locator matches exactly one element or the step does not proceed, and there are no sleeps — only condition waits with a deadline (10 s, polled every 200 ms). Every action is verified: values are read back from the field, a step that changed screens must reach that screen (`expect`), extracted text must parse as its declared type, every output must be present, and the success checkpoint must hold.

**Runtime states are product knowledge.** The model only walks the happy path, so it cannot discover "not found". Known states live in the **app profile** (`profiles/meridian-core.json`) as a signature plus a class, authored once per product and shared by every capability and tenant. Replay checks them on every poll of every wait, *before* looking for its own target, so an exceptional state is recognised as itself rather than as a timeout, and nothing is clicked while one is on screen.

| Class | In the profile | Response | Result |
|---|---|---|---|
| business | `record_not_found`, `validation_error`, `permission_denied` | stop | `business_outcome` + the application's message |
| recoverable | `system_notice` → dismiss; `session_expired`, `app_error` → restart from entry in a fresh session | bounded by `max_attempts` per state and two restarts per run | `success`, listed under `recoveries` (with any wait over 2.5 s, as `slow_response`) |
| needs_human | `supervisor_override` | escalate (§5) | continues, or `failed: needs_human` |
| fatal | `signon_rejected` | stop | `failed: fatal_state` |
| *unknown* | a control or expected screen never appears | escalate as *stuck* if an operator is attached, else stop | `failed` with step, expected, observed, screenshot, observation |
| *unknown* | a value does not parse or read back; the checkpoint or an output is missing | stop | same |

**At most once.** After a committing step is dispatched — by automation or by a human on its behalf — the engine will not restart the flow or repeat the step, whatever appears. If the effect cannot be verified the result is `failed: indeterminate` with `irreversible_step_dispatched: true` (or an operator is asked to look), never a silent retry.

**UI drift (secondary).** The profile's fingerprint must match before the first action (`app_mismatch`). If a primary locator misses and a fallback matches, the run continues and reports it under `drift`; positional fallbacks are never used for steps recorded as committing. The step's `expect` then checks where the fallback led: in `evidence/09-replay-tenant-drift` the positional fallback happens to hit the right menu cell, but the screen it opens is titled differently from what was recorded, so the run stops there instead of clicking on through an unmapped tenant.

## 4. Heterogeneity & multi-tenant

**The seam** is `surface.Surface`: `Observe()` returns elements (role, name, value, rectangle, frame); `Click/Type/Select` take an element; `Pointer/Keys/Press` are raw input for a human. The artifact and the engine speak only in those terms. A second, in-memory implementation (`internal/surface/fake`) runs the engine's tests — the cheapest proof the seam holds.

- **Legacy web** is what is implemented: a frameset of layout tables with no ids, unlabelled fields, `onclick` table cells and a native confirm dialog (surfaced as a synthetic `dialog` frame, as a desktop modal would be).
- **Desktop.** UI Automation and the macOS AX API are also role + name + rectangle, so an adapter returns the same observation and acts through OS input; locators are unchanged. A `Screen` names a window and title instead of a frame and path, and with no network layer, risk control rests on label rules and approval alone.
- **Pixels only** (Citrix, canvas). `Observe` becomes OCR plus control detection. `label` and `grid` locators are geometry over text rectangles and survive; `name` weakens. A vision model belongs here, inside the adapter, not in replay.

**Multi-tenant reuse.** Three files, three lifetimes: the **artifact** per product capability; the **profile** per product version; the **tenant binding** for everything institution-specific — base URL, secret references, and `labels`, a map from the product's canonical wording to the tenant's. At run time the artifact is *localized* through the binding, never copied or edited; recording on a tenant with overrides maps labels back to canonical. `evidence/08` replays the Pinecrest-recorded artifact unchanged on a second, differently-worded tenant.

**Drift.** Detection is in the result contract: fingerprint mismatch, fallback-locator use, failed step checkpoints. Wording drift is fixed in the tenant binding; a product release bumps the profile version, and artifacts recorded against another version are refused until re-validated. Next: a scheduled read-only canary replay per tenant, a stability score per artifact and tenant, and step-level overrides for tenants whose flow, not just wording, differs.

## 5. Escalation & handoff

**Detecting "stuck".** Four triggers, one intervention request: *stuck* (replay: a control or expected screen does not appear and no known state explains it; discovery: three consecutive rejected or ineffective actions, or the model asks); *approval* (an irreversible step); *needs_human* (a state the profile marks human-only); *indeterminate* (a committing step whose effect could not be verified). The request carries capability, goal, step and intent, the reason, what automation expected, the redacted screen text and a redacted screenshot.

**Control transfer.** `control.Session` wraps the surface with one control token: `automation → pending_human → human(operator) → automation`. Every action checks the token, so automation cannot act while a human holds the session and a human cannot act before claiming it; a transfer waits for any in-flight action, increments an epoch and is logged. The operator drives the **same browser session** with raw input (click a point on the live view, type, press a key). Each human action is recorded: the control it landed on (by hit-testing the observation) and the *length* of typed text — never the text, which may be exactly the credential automation is not allowed to hold.

**Handing back** is `approve`/`deny` (no control transfer needed), `resume` ("I cleared it — carry on where you paused"), `step_done` ("I did your step by hand") or `abort`. After `resume`, automation lets the screen settle, re-observes and re-evaluates the wait it was in, so the human's work is verified by its own checkpoint rather than trusted. `evidence/07` shows an operator entering a supervisor override mid-replay, then approving the committing step. A discovery run in which a human acted is marked `human_assisted`, because those actions are not recorded steps.

**Mocked:** the console is one HTML page polling a screenshot, loopback only, with a free-text operator name. **Not built:** operator authentication and queueing, pre-emptive takeover of a session that did not ask, live streaming.

## 6. Safety

- **Allowlist, enforced twice.** The profile lists permitted paths and action types; the tenant supplies the origin. It is checked per action and again at the network layer, where every request the browser makes passes a guard that fails anything outside it. The model has no "navigate to a URL" action.
- **Risky actions require a human by default.** A click is irreversible if its control name matches the profile's rules, *and* committing routes are held at the network layer unless a step was authorized, so a mislabelled button still cannot commit. The wire is the source of truth: in the recorded write flow the POST is actually sent by the dialog's "OK", a step the label rules call reversible, and it is the guard — armed by the approval of "Confirm" one step earlier — that lets exactly that one request through. Discovery always needs an operator's approval for such a step. Replay does too, unless the artifact is approved **and** the caller explicitly authorizes the commit; a draft committing artifact does not run unattended at all. I chose confirmation over blocking because write capabilities are the point of the system, and over flagging because a flag after a posted transaction is too late.
- **Data.** Secrets are names in the artifact and environment lookups at run time; the model is given placeholders. One redactor guards every exit from the live session — model input, log, saved observations, screenshots — masking by pattern, by adjacent label, and by literal for the run's secrets and sensitive inputs and outputs. Outputs go to the caller; the log holds `[output:name]`.

**Limits.** Redaction is only as good as the profile: free-text PII that no pattern or label covers gets through. That happened while building this — a member name in an unlabelled sentence reached a saved observation until a profile pattern was added for it — and literal masking skips values shorter than four characters. The model sees screen structure and unmasked non-sensitive text, so discovery belongs on a sandbox tenant. The operator's live view is unredacted by design. Risk rules are a curated list: a committing control or route missing from the profile is not caught, and on a surface with no network layer only the label rules remain.

## 7. Cuts

| Cut | Why | Next |
|---|---|---|
| Sign-on recorded inline in every artifact | Self-contained, restartable artifacts | A shared `requires: session` sub-capability per profile |
| Known states hand-authored in the profile | A happy-path run cannot discover them | "Negative discovery": run the agent on bad inputs, propose signatures for review |
| No trajectory minimization; human actions in discovery not turned into steps | The model took direct paths; typed text is unknowable by design | Drop no-op steps; a `manual` step type that always escalates |
| No model-assisted recovery in replay | Replay stays strictly model-free | One bounded, policy-checked model step on `target_not_found`, recorded as evidence |
| Console, operator identity, secret store, desktop surface: mocks or seams | Out of scope here | README "What is mocked" |
| No queue, scheduler, registry, per-tenant step overrides | Infrastructure before the abstractions are proven is premature | Canary replays and a stability score first |

Stretch goals touched because they fell out of the design: cross-tenant reuse with overrides (§4), an approval state (§6), a capability catalog (`cua capabilities`).
