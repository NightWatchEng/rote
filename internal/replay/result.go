// Package replay is the production execution path: it runs a saved artifact
// against a live session with no model anywhere in the loop, and returns a
// structured result.
package replay

import "github.com/NightWatchEng/rote/internal/control"

// Status is the top-level answer to "what happened?". The three values are
// deliberately different kinds of thing, and a caller should branch on them.
type Status string

const (
	// StatusSuccess: the flow ran, the checkpoint held, outputs are attached.
	StatusSuccess Status = "success"
	// StatusOutcome: the application gave a legitimate answer that is not the
	// happy path ("no such member"). Nothing failed; do not retry.
	StatusOutcome Status = "business_outcome"
	// StatusFailed: the run could not be completed. Failure says where and why.
	StatusFailed Status = "failed"
)

// Failure kinds.
const (
	FailInvalidInput    = "invalid_input"         // caller error; the application was never touched
	FailConfiguration   = "configuration"         // missing secret, unusable artifact/tenant pairing
	FailNotApproved     = "not_approved"          // a draft committing capability cannot run unattended
	FailAppMismatch     = "app_mismatch"          // the session is not showing the product the artifact targets
	FailTargetNotFound  = "target_not_found"      // a step's control never appeared and no known state explains it
	FailCheckpoint      = "checkpoint_failed"     // a step or the final checkpoint was not reached
	FailReadback        = "readback_mismatch"     // a field did not hold what was entered
	FailOutputMissing   = "output_missing"        // a declared output was never read
	FailOutputInvalid   = "output_invalid"        // an extracted value is not of its declared type
	FailUnexpectedState = "unexpected_state"      // a known state appeared that this capability does not declare
	FailFatalState      = "fatal_state"           // the application is in a known dead end
	FailRecovery        = "recovery_exhausted"    // a recoverable condition kept recurring
	FailPolicy          = "policy_violation"      // the run tried to leave the allowlist
	FailApprovalNeeded  = "approval_required"     // a committing step needs a human and none is attached
	FailDenied          = "denied_by_operator"    // a human declined the committing step
	FailAborted         = "aborted_by_operator"   // a human stopped the run
	FailNeedsHuman      = "needs_human"           // a human-only state appeared and none is attached
	FailUnanswered      = "escalation_unanswered" // nobody resolved the intervention in time
	FailIndeterminate   = "indeterminate"         // a committing step was dispatched and could not be verified
	FailSurface         = "surface_error"         // the browser/session itself failed
)

// Result is the replay contract: what a calling agent gets back.
type Result struct {
	Status     Status `json:"status"`
	Capability string `json:"capability"`
	Version    string `json:"version"`
	Tenant     string `json:"tenant"`
	RunID      string `json:"run_id"`

	// Outputs holds the declared outputs on success.
	Outputs map[string]any `json:"outputs,omitempty"`
	// Outcome is set when Status is business_outcome.
	Outcome *Outcome `json:"outcome,omitempty"`
	// Failure is set when Status is failed.
	Failure *Failure `json:"failure,omitempty"`

	// The remaining fields describe how the run went, whatever its status.

	// Recoveries lists recoverable conditions the engine got past.
	Recoveries []Recovery `json:"recoveries,omitempty"`
	// Drift lists steps whose primary locator no longer matched and a
	// fallback was used: the run worked, but the artifact is ageing.
	Drift []Drift `json:"drift,omitempty"`
	// Handoffs lists every time a human was brought in.
	Handoffs []control.Record `json:"handoffs,omitempty"`

	StepsCompleted int    `json:"steps_completed"`
	StepsTotal     int    `json:"steps_total"`
	Attempts       int    `json:"attempts"`
	DurationMS     int64  `json:"duration_ms"`
	Evidence       string `json:"evidence"`
}

// Outcome is an expected business result.
type Outcome struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	AtStep  string `json:"at_step,omitempty"`
}

// Failure says what step, what was expected and what was observed.
type Failure struct {
	Kind     string `json:"kind"`
	Step     string `json:"step,omitempty"`
	Intent   string `json:"intent,omitempty"`
	Expected string `json:"expected,omitempty"`
	Observed string `json:"observed,omitempty"`
	Message  string `json:"message"`
	// IrreversibleDispatched is true if a committing step had already been
	// sent when the run failed. If so the run must not simply be retried.
	IrreversibleDispatched bool   `json:"irreversible_step_dispatched"`
	Screenshot             string `json:"screenshot,omitempty"`
	Observation            string `json:"observation,omitempty"`
}

// Recovery is one recoverable condition that was handled.
type Recovery struct {
	Condition string `json:"condition"`
	Action    string `json:"action"`
	AtStep    string `json:"at_step,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	WaitedMS  int64  `json:"waited_ms,omitempty"`
}

// Drift is one use of a fallback locator.
type Drift struct {
	Step   string   `json:"step"`
	Used   string   `json:"used"`
	Missed []string `json:"missed"`
}
