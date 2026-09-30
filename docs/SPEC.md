# Specification

The requirements this system implements, each with its acceptance criterion, and the traceability from every requirement to where it is implemented, tested and demonstrated. The design write-up in [../REPORT.md](../REPORT.md) explains the decisions; this document says what had to be true.

## 1. Problem statement

AI agents that serve banks and credit unions need to operate the institutions' back-office applications. Where an application has an API, integration goes through the API. The long tail does not: core banking screens, servicing tools and admin consoles whose only interface is the one a human operator uses. This system exists for that case.

A model is allowed to work out **once** how a task is done on such a surface. What it learns is captured as a reusable, reviewable, parameterized **capability** that agents then invoke through **deterministic replay**, with no model in the loop. When replay cannot safely continue, a **human operator takes over the same live session** and hands it back.

## 2. Environment assumptions

| ID | Assumption | Consequence for the design |
|---|---|---|
| E1 | Application UIs are stable and change slowly; the hard part is runtime conditions, not layout drift: validation errors, "record not found", permission denials, unexpected dialogs, session expiry, slowness, application errors. | Replay must recognise and respond to those conditions deliberately; drift handling is secondary. |
| E2 | Surfaces are heterogeneous and often legacy: modern web, server-rendered web with framesets, nested tables, no test ids, or native desktop. No clean DOM, stable selectors or API can be assumed. | Perception and action must work from what an operator sees: roles, names, rectangles. |
| E3 | Hundreds of institutions run about twenty applications each; many run the same vendor product configured, branded and versioned differently. | A capability must be reusable across institutions running the same product, with per-institution specialisation, and drift must be detectable. |
| E4 | The data is regulated financial data. | Credentials, tokens and personal data must never be persisted into artifacts or logs. |

## 3. Requirements

### R1 — Goal-driven discovery

| ID | Requirement | Acceptance |
|---|---|---|
| R1.1 | Accept a goal in natural language plus a target (application, entry point). | The goal is given as a sentence; the target is a tenant binding with a base URL and an entry path. |
| R1.2 | Run a model-driven observe → decide → act loop against a live surface until the goal is met or a stopping condition is hit: step budget, time budget, dead end. | All four stopping conditions exist and each ends the run with a stated reason. |
| R1.3 | The model must interact with a real UI (click, type, navigate, read state) through a mechanism that would still work without a clean DOM. | No CSS or XPath selectors anywhere; perception is the accessibility tree, action is synthesized input at coordinates. |
| R1.4 | The model never sees a secret and never chooses how a control is located. | Secrets appear to the model only as placeholders; locators are derived and verified by the harness. |

### R2 — Capability artifact

| ID | Requirement | Acceptance |
|---|---|---|
| R2.1 | After a successful run, emit a typed, serializable, versioned artifact decoupled from the model transcript. | JSON with `schema_version` and a capability `version`; no transcript or concrete run data inside. |
| R2.2 | The artifact expresses ordered steps, how each target is identified with reasoning about robustness, typed inputs, typed outputs, and a success checkpoint. | Every step has an ordered locator list with a rationale; inputs and outputs are typed; `expect` per navigating step and `success` at the end. |
| R2.3 | The artifact is a contract an agent can call: it declares the non-success outcomes a caller must handle. | Outcome codes are part of the artifact and exported as a tool definition. |
| R2.4 | The artifact is reviewable by a person and gated by approval. | A review rendering exists; approval is bound to the reviewed content by a digest. |
| R2.5 | The artifact contains no concrete input or secret value. | Saving is refused if one is found. |

### R3 — Deterministic replay

| ID | Requirement | Acceptance |
|---|---|---|
| R3.1 | Replay a saved artifact with a set of inputs without invoking a model for any decision. | The replay engine has no model dependency. |
| R3.2 | Use stable targeting, verify the checkpoint, return the declared outputs. | A locator must match exactly one element; typed read-back and parsing; every output present before success. |
| R3.3 | Detect runtime conditions and respond deliberately, distinguishing expected business outcomes, recoverable conditions and hard failures in the result contract. | Result status is one of `success`, `business_outcome`, `failed`; recoveries, drift and handoffs are reported alongside. |
| R3.4 | A failure is debuggable: which step, what was expected, what was observed, plus a richer signal. | Failure carries step, expected, observed, a screenshot and an observation snapshot. |
| R3.5 | A committing (irreversible) step is never repeated and the flow is never restarted after one. | Enforced in the engine and at the network layer; verified by tests. |

### R4 — Safety and policy

| ID | Requirement | Acceptance |
|---|---|---|
| R4.1 | Enforce an explicit, configurable allowlist of destinations and action types; the automation must not act outside it. | Checked per action and enforced for every request at the network layer. |
| R4.2 | Distinguish safe/reversible from risky/irreversible actions and handle the risky class conservatively. | Irreversible steps require a human's approval unless the artifact is approved and the caller explicitly authorizes the commit. |
| R4.3 | Never persist secrets or raw sensitive data into artifacts or logs; redact appropriately. | One redactor in front of the model, the log, saved observations and screenshots; secrets resolved only at the moment of typing. |

### R5 — Evidence

| ID | Requirement | Acceptance |
|---|---|---|
| R5.1 | Produce a structured log of what the automation did and why. | An append-only event log per run with actor, event, step and the model's stated reason for each decision. |
| R5.2 | Produce at least one richer signal on failure. | Redacted screenshot and observation snapshot at failures, outcomes and interventions. |
| R5.3 | Keep a demonstration of the whole flow: a saved artifact, a discovery run, a replay run, and a replay that hits an exceptional state. | The `evidence/` directory, indexed. |

### R6 — Human escalation and handoff

| ID | Requirement | Acceptance |
|---|---|---|
| R6.1 | Detect a stuck or blocked state and raise an intervention request carrying enough context to act on: capability, goal, step, current state, reason. | One request type for four triggers: stuck, approval, human-only state, indeterminate commit. |
| R6.2 | Let the human operate the same live session — not a fresh one — and then hand control back so the run resumes. | A control token over the one session; the operator drives it through raw input; hand-back resolutions resume, complete the step, approve, deny or abort. |
| R6.3 | Preserve context and evidence across the handoff and record what the human did. | Every transfer is logged with an epoch; human actions are recorded (never typed text). |
| R6.4 | It must always be knowable who is, or should be, in control. | The control state is exposed and every action is checked against it. |
| R6.5 | The operator surface may be minimal or mocked; the handoff mechanism and control-transfer model must be real. | A bare console and a command-line operator over the same API. |

### R7 — Heterogeneity and scale (design)

| ID | Requirement | Acceptance |
|---|---|---|
| R7.1 | A clear seam between "how we perceive and act on a surface" and "the recorded flow", credible for legacy web and desktop. | A surface interface with a second, in-memory implementation; the design write-up covers desktop and pixel-only surfaces. |
| R7.2 | Represent a capability so it can be reused or safely specialised across institutions running the same product; detect and manage per-institution and per-version drift. | Product profile, per-institution binding with overrides, artifact localized at run time; drift reported by the result contract; demonstrated on a second tenant. |

## 4. Non-goals

Scaling infrastructure (queues, clusters, multi-tenant plumbing), a production operator console, desktop and pixel-only surface implementations, and a secret store. Each is left as a documented seam.

## 5. Traceability

| ID | Implemented in | Tested by | Shown in |
|---|---|---|---|
| R1.1 | `cmd/cua` (`discover --goal`), `internal/agent/prompt.go` | `internal/agent` tests | `evidence/01`, README demo step 1, `docs/demo.mp4` |
| R1.2 | `internal/agent/agent.go` (`loop`) | `TestDiscoverStuck`, turn-budget tests | `evidence/01`, `06` |
| R1.3 | `internal/surface/web` | `internal/e2e` | every run |
| R1.4 | `internal/agent` (`checkTemplate`, `perform`), `internal/redact` | agent and redact tests | `evidence/01/events.jsonl` |
| R2.1–R2.2 | `internal/capability/artifact.go`, `internal/locate` | capability and locate tests | `artifacts/*.json`, `cua describe` |
| R2.3 | `capability.Tool`, `cua capabilities` | capability tests | README "Agent-facing catalog" |
| R2.4 | `capability.Describe`, `Approval` digest, `cua approve` | capability tests | README "Safety" |
| R2.5 | `internal/agent/agent.go` (`artifact`) | agent tests | — |
| R3.1 | `internal/replay` (no model import) | `go list -deps` | `evidence/02` |
| R3.2 | `internal/replay/engine.go` (`act`, `await`), `internal/locate` | replay and e2e tests | `evidence/02`, `08` |
| R3.3 | `internal/replay/result.go`, `internal/capability/profile.go` (states) | replay and e2e tests | `evidence/03`, `04`, `05`, `10` |
| R3.4 | `replay.Failure`, `runner.Kit.Snapshot` | e2e test on member 10063 | `evidence/05`, `09` |
| R3.5 | `engine.go` (`committed`, `indeterminate`), `policy.Guard` | `TestSubAccountNoRetryAfterCommit`, replay tests | REPORT §3 |
| R4.1 | `internal/policy`, `surface/web` request gate | policy, guard and e2e tests | README "Safety" |
| R4.2 | `policy.ClassifyControl`, `Guard.Arm`, engine approval path | replay and e2e tests | `evidence/06`, `07` |
| R4.3 | `internal/redact`, `capability.Environment.Secrets` | redact tests, evidence hygiene e2e test | every run's log and screenshots |
| R5.1–R5.2 | `internal/evidence`, `runner.Kit` | e2e tests | `evidence/*` |
| R5.3 | `scripts/evidence.sh` | — | `evidence/README.md` |
| R6.1 | `control.Request`, `runner.Kit.Escalate` | control tests | `evidence/07` |
| R6.2–R6.4 | `internal/control`, `internal/operator` | control and operator tests, e2e override test | `evidence/07`, `docs/console.mp4` |
| R6.5 | `internal/operator/console.html`, `cua operator` | operator tests | README "Human handoff" |
| R7.1 | `internal/surface`, `internal/surface/fake` | replay and agent tests run on the fake | REPORT §4 |
| R7.2 | `internal/capability/profile.go` (`Environment`, `Localize`) | capability tests, e2e tenant-variant test | `evidence/08`, `09` |
