package replay_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/replay"
	"github.com/NightWatchEng/rote/internal/surface"
	"github.com/NightWatchEng/rote/internal/surface/fake"
)

const confirm = "Confirm Transfer"

// unattended is how a caller runs an approved committing capability.
var unattended = replay.Options{AuthorizeIrreversible: true}

// showWhen makes a click (by automation or by an operator's pointer) on a
// named control lead to another screen, for the nth and later hits (n = 0:
// every time).
func showWhen(app *fake.App, control, screen string, only int) {
	app.OnAction(func(a fake.Action, e *fake.Effect) {
		if (a.Kind == "click" || a.Kind == "pointer") && a.Name == control && (only == 0 || a.N == only) {
			e.Show = screen
		}
	})
}

func hasRecovery(res *replay.Result, condition, action string) bool {
	return slices.ContainsFunc(res.Recoveries, func(r replay.Recovery) bool { return r.Condition == condition && r.Action == action })
}

func TestSuccessReturnsDeclaredOutputs(t *testing.T) {
	r := newRig(t, config{})
	res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)

	wantStatus(t, res, replay.StatusSuccess)
	if got := res.Outputs["confirmation"]; got != "TX-12345" {
		t.Errorf("confirmation output %v, want TX-12345", got)
	}
	if res.StepsCompleted != 6 || res.StepsTotal != 6 || res.Attempts != 1 {
		t.Errorf("steps %d/%d in %d attempt(s), want 6/6 in 1", res.StepsCompleted, res.StepsTotal, res.Attempts)
	}
	if res.Capability != "funds.transfer" || res.Version != "1.0.0" || res.Tenant != "lakeside" || res.RunID == "" {
		t.Errorf("result does not identify the run: %s", dump(res))
	}
	if res.Failure != nil || res.Outcome != nil || len(res.Drift) > 0 || len(res.Handoffs) > 0 {
		t.Errorf("a clean run reported more than success: %s", dump(res))
	}
	if n := r.app.Count("click", confirm); n != 1 {
		t.Errorf("committing control clicked %d times, want 1", n)
	}
	typed := slices.ContainsFunc(r.app.Actions(), func(a fake.Action) bool { return a.Kind == "type" && a.Text == amount })
	if !typed {
		t.Error("the amount input was never typed")
	}
}

// ---- committing steps are never repeated ----

func TestRestartClassStateAfterCommitIsIndeterminate(t *testing.T) {
	r := newRig(t, config{})
	showWhen(r.app, confirm, "error", 0)
	res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)

	f := wantFailure(t, res, replay.FailIndeterminate)
	if !f.IrreversibleDispatched {
		t.Error("failure does not say a committing step was dispatched")
	}
	if res.Attempts != 1 || r.app.Count("open", "") != 1 {
		t.Errorf("flow restarted: attempts %d, opens %d", res.Attempts, r.app.Count("open", ""))
	}
	if n := r.app.Count("click", confirm); n != 1 {
		t.Errorf("committing control clicked %d times, want exactly 1", n)
	}
}

func TestOperatorResumeAfterCommitDoesNotSkipCheckpoint(t *testing.T) {
	// The operator keeps saying "carry on" while the error screen is still
	// up. Each time the interrupted wait is re-evaluated, so the run cannot
	// get past a screen that does not show the transfer went through.
	resumes := func(n int, _ *control.Intervention) reply {
		if n <= 3 {
			return reply{action: control.Resume}
		}
		return reply{}
	}
	r := newRig(t, config{answer: resumes})
	showWhen(r.app, confirm, "error", 0)
	res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)

	if res.Status == replay.StatusSuccess {
		t.Fatalf("run reported success while the success condition does not hold:\n%s", dump(res))
	}
	f := wantFailure(t, res, replay.FailIndeterminate)
	if !f.IrreversibleDispatched {
		t.Error("failure does not say a committing step was dispatched")
	}
	reqs := r.op.requests()
	if len(reqs) != 4 {
		t.Fatalf("got %d intervention(s), want each resume to escalate again until one goes unanswered", len(reqs))
	}
	for _, q := range reqs {
		if q.Kind != control.KindIndeterminate || q.StepID != "s5" {
			t.Errorf("intervention %s at %s, want indeterminate at the committing step s5", q.Kind, q.StepID)
		}
	}
	if n := r.app.Count("click", confirm); n != 1 {
		t.Errorf("committing control clicked %d times, want exactly 1", n)
	}
	if res.Attempts != 1 || r.app.Count("open", "") != 1 {
		t.Errorf("flow restarted: attempts %d, opens %d", res.Attempts, r.app.Count("open", ""))
	}
}

func TestCommitDoneByHandCountsAsCommitted(t *testing.T) {
	cases := []struct {
		name string
		kind control.Kind
		edit func(map[string]fake.Screen)
		opts replay.Options
	}{
		// Approved but not authorized: automation asks, the operator commits.
		{name: "approval", kind: control.KindApproval},
		// The committing control is not where the artifact expects it; the
		// operator finds it and commits by hand.
		{name: "stuck", kind: control.KindStuck, opts: unattended,
			edit: func(s map[string]fake.Screen) { rename(s, "transfer", confirm, "Confirm Xfer") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, config{edit: tc.edit,
				answer: firstOnly(reply{action: control.StepDone, inputs: []control.Input{confirmAt}})})
			// Whatever the operator clicked, the app then fails in a way that
			// would ordinarily restart the flow.
			showWhen(r.app, confirm, "error", 0)
			showWhen(r.app, "Confirm Xfer", "error", 0)
			res := r.run(approve(transferArtifact(t)), goodInputs(), tc.opts)

			f := wantFailure(t, res, replay.FailIndeterminate)
			if !f.IrreversibleDispatched {
				t.Error("a step committed by hand is not reported as dispatched")
			}
			if reqs := r.op.requests(); len(reqs) == 0 || reqs[0].Kind != tc.kind {
				t.Fatalf("interventions %v, want the first to be %s", reqs, tc.kind)
			}
			pointer := slices.IndexFunc(r.app.Actions(), func(a fake.Action) bool { return a.Kind == "pointer" })
			if pointer < 0 || !strings.HasPrefix(r.app.Actions()[pointer].Name, "Confirm") {
				t.Fatalf("the operator's click did not reach the committing control: %+v", r.app.Actions())
			}
			if n := r.app.Count("click", confirm) + r.app.Count("click", "Confirm Xfer"); n != 0 {
				t.Errorf("automation clicked the committing control %d time(s) after a person committed it", n)
			}
			if res.Attempts != 1 || r.app.Count("open", "") != 1 {
				t.Errorf("flow restarted: attempts %d, opens %d", res.Attempts, r.app.Count("open", ""))
			}
		})
	}
}

// ---- approval ----

func TestApprovalSurvivesTenantLabels(t *testing.T) {
	labels := map[string]string{confirm: "Submit Transfer", "Amount:": "Transfer Amount:", "Transfer Complete": "Transfer Posted"}
	r := newRig(t, config{labels: labels})
	art := approve(transferArtifact(t))

	// The precondition that made this a defect: localizing changes the digest.
	local, err := r.kit.Env.Localize(art)
	if err != nil {
		t.Fatal(err)
	}
	if local.Digest() == art.Digest() {
		t.Fatal("tenant labels did not change the artifact; the test would prove nothing")
	}

	res := r.run(art, goodInputs(), unattended)
	wantStatus(t, res, replay.StatusSuccess)
	if n := r.app.Count("click", "Submit Transfer"); n != 1 {
		t.Errorf("tenant's committing control clicked %d times, want 1", n)
	}
}

func TestCommittingStepGate(t *testing.T) {
	t.Run("draft without operator is refused before opening", func(t *testing.T) {
		r := newRig(t, config{})
		res := r.run(transferArtifact(t), goodInputs(), unattended)
		wantFailure(t, res, replay.FailNotApproved)
		if n := len(r.app.Actions()); n != 0 {
			t.Errorf("the application received %d action(s); want it never opened", n)
		}
	})
	t.Run("approved but not authorized by the caller", func(t *testing.T) {
		r := newRig(t, config{})
		res := r.run(approve(transferArtifact(t)), goodInputs(), replay.Options{})
		f := wantFailure(t, res, replay.FailApprovalNeeded)
		if f.Step != "s5" || f.IrreversibleDispatched {
			t.Errorf("failure at %s (dispatched=%v), want s5 before dispatch", f.Step, f.IrreversibleDispatched)
		}
		if n := r.app.Count("click", confirm); n != 0 {
			t.Errorf("committing control clicked %d times without approval", n)
		}
	})
	t.Run("operator denies", func(t *testing.T) {
		r := newRig(t, config{answer: always(control.Deny)})
		res := r.run(transferArtifact(t), goodInputs(), replay.Options{})
		wantFailure(t, res, replay.FailDenied)
		if n := r.app.Count("click", confirm); n != 0 {
			t.Errorf("committing control clicked %d times after a denial", n)
		}
	})
	t.Run("operator approves", func(t *testing.T) {
		r := newRig(t, config{answer: always(control.Approve)})
		res := r.run(transferArtifact(t), goodInputs(), replay.Options{})
		wantStatus(t, res, replay.StatusSuccess)
		if n := r.app.Count("click", confirm); n != 1 {
			t.Errorf("committing control clicked %d times, want exactly 1", n)
		}
		if len(res.Handoffs) != 1 || res.Handoffs[0].Kind != control.KindApproval || res.Handoffs[0].Resolution != control.Approve {
			t.Errorf("handoffs %s, want one approved approval", dump(res.Handoffs))
		}
	})
}

func TestLiveControlIsReclassified(t *testing.T) {
	// The artifact claims the commit is harmless; the policy recognises the
	// control by its name and still demands approval.
	art := transferArtifact(t)
	art.Steps[4].Risk = policy.RiskReversible
	art.Risk = policy.RiskReversible
	mustValidate(t, art)

	r := newRig(t, config{})
	res := r.run(art, goodInputs(), unattended)
	f := wantFailure(t, res, replay.FailApprovalNeeded)
	if f.Step != "s5" {
		t.Errorf("stopped at %s, want s5", f.Step)
	}
	if n := r.app.Count("click", confirm); n != 0 {
		t.Errorf("committing control clicked %d times", n)
	}
}

func TestCommittingStepNeverUsesOrdinalLocator(t *testing.T) {
	art := transferArtifact(t)
	// Button #2 on the transfer screen is the committing control, so a
	// positional fallback would find it.
	art.Steps[4].Target.Locators = append(art.Steps[4].Target.Locators,
		locate.Locator{Strategy: locate.ByOrdinal, Role: surface.RoleButton, Index: 2})
	approve(art)

	r := newRig(t, config{edit: func(s map[string]fake.Screen) { rename(s, "transfer", confirm, "Confirm Xfer") }})
	res := r.run(art, goodInputs(), unattended)
	f := wantFailure(t, res, replay.FailTargetNotFound)
	if f.Step != "s5" || f.IrreversibleDispatched {
		t.Errorf("failure at %s (dispatched=%v), want s5 before dispatch", f.Step, f.IrreversibleDispatched)
	}
	if n := r.app.Count("click", "Confirm Xfer"); n != 0 {
		t.Errorf("committing control clicked %d times through a positional locator", n)
	}
}

// ---- outputs and outcomes ----

func TestExtractDoneByHandIsOutputMissing(t *testing.T) {
	r := newRig(t, config{
		edit:   func(s map[string]fake.Screen) { rename(s, "done", "Confirmation No:", "Reference:") },
		answer: firstOnly(reply{action: control.StepDone}),
	})
	res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)

	f := wantFailure(t, res, replay.FailOutputMissing)
	if f.Step != "s6" {
		t.Errorf("failed at %s, want the extract step s6", f.Step)
	}
	if reqs := r.op.requests(); len(reqs) != 1 || reqs[0].Kind != control.KindStuck {
		t.Errorf("interventions %v, want one stuck request", reqs)
	}
}

func TestBusinessOutcomeCarriesNoOutputs(t *testing.T) {
	// Read a value first, then hit a business outcome: the value read so far
	// is not part of the answer.
	full := transferArtifact(t)
	read := step("s3", capability.ActionExtract, labelled(surface.RoleText, "Available:"), policy.RiskRead)
	read.Output = "available"
	details := step("s4", capability.ActionClick, named(surface.RoleButton, "Details"), policy.RiskReversible)
	details.Expect = &locate.Screen{Frame: "main", Path: "/details"}
	art := full
	art.ID, art.Inputs = "member.details", nil
	art.Outputs = []capability.Output{{Name: "available", Type: "money"}}
	art.Steps = []capability.Step{full.Steps[0], full.Steps[1], read, details}
	art.Success = locate.Condition{Text: "Details", Frame: "main"}
	art.Risk = policy.RiskReversible
	mustValidate(t, art)

	r := newRig(t, config{})
	res := r.run(art, map[string]string{}, replay.Options{})
	wantStatus(t, res, replay.StatusOutcome)
	if res.Outcome == nil || res.Outcome.Code != "record_not_found" || res.Outcome.AtStep != "s4" {
		t.Errorf("outcome %s, want record_not_found at s4", dump(res.Outcome))
	}
	if res.Outputs != nil {
		t.Errorf("business outcome carries outputs %v", res.Outputs)
	}
	if res.StepsCompleted != 3 {
		t.Errorf("steps completed %d, want 3 (the extract ran before the outcome)", res.StepsCompleted)
	}
}

// ---- recovery ----

func TestRecoveries(t *testing.T) {
	t.Run("dismissable interruption", func(t *testing.T) {
		r := newRig(t, config{})
		showWhen(r.app, "Sign On", "notice", 1)
		res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
		wantStatus(t, res, replay.StatusSuccess)
		if !hasRecovery(res, "system_notice", capability.RecoverDismiss) {
			t.Errorf("recoveries %s, want system_notice dismissed", dump(res.Recoveries))
		}
		if n := r.app.Count("click", "OK"); n != 1 {
			t.Errorf("dismiss control clicked %d times, want 1", n)
		}
		if res.Attempts != 1 {
			t.Errorf("attempts %d, want 1", res.Attempts)
		}
	})
	t.Run("restart before any commit", func(t *testing.T) {
		r := newRig(t, config{})
		showWhen(r.app, "Sign On", "error", 1)
		res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
		wantStatus(t, res, replay.StatusSuccess)
		if res.Attempts != 2 || r.app.Count("open", "") != 2 {
			t.Errorf("attempts %d, opens %d; want the flow started over once", res.Attempts, r.app.Count("open", ""))
		}
		if !hasRecovery(res, "app_error", capability.RecoverRestart) {
			t.Errorf("recoveries %s, want app_error restart", dump(res.Recoveries))
		}
		if n := r.app.Count("click", confirm); n != 1 {
			t.Errorf("committing control clicked %d times, want 1", n)
		}
	})
	t.Run("restart is bounded", func(t *testing.T) {
		r := newRig(t, config{})
		showWhen(r.app, "Sign On", "error", 0)
		res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
		wantFailure(t, res, replay.FailRecovery)
		if opens := r.app.Count("open", ""); opens != 2 {
			t.Errorf("opens %d, want 2 (the profile allows one restart)", opens)
		}
		if n := r.app.Count("click", confirm); n != 0 {
			t.Errorf("committing control clicked %d times", n)
		}
	})
	t.Run("slow screen", func(t *testing.T) {
		r := newRig(t, config{})
		r.app.OnAction(func(a fake.Action, e *fake.Effect) {
			if a.Kind == "click" && a.Name == "Sign On" {
				e.Show = "loading"
				time.AfterFunc(80*time.Millisecond, func() { r.app.Show("transfer") })
			}
		})
		res := r.run(approve(transferArtifact(t)), goodInputs(), replay.Options{AuthorizeIrreversible: true, StepTimeout: time.Second})
		wantStatus(t, res, replay.StatusSuccess)
		if res.Attempts != 1 {
			t.Errorf("attempts %d, want 1", res.Attempts)
		}
	})
}

// ---- read-back ----

// truncateTyping makes the field keep one character less than was typed, as
// a field with a too-short maxlength would.
func truncateTyping(app *fake.App, value string) {
	app.OnAction(func(a fake.Action, e *fake.Effect) {
		if a.Kind == "type" && a.Text == value {
			e.Text = value[:len(value)-1]
		}
	})
}

func TestReadbackMismatch(t *testing.T) {
	t.Run("plain value is quoted", func(t *testing.T) {
		r := newRig(t, config{})
		truncateTyping(r.app, amount)
		res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
		f := wantFailure(t, res, replay.FailReadback)
		if f.Step != "s4" || !strings.Contains(f.Expected, amount) || !strings.Contains(f.Observed, `"250.0"`) {
			t.Errorf("failure %s, want s4 quoting what was typed and what landed", dump(f))
		}
		if n := r.app.Count("click", confirm); n != 0 {
			t.Error("the flow went on to commit after input did not land")
		}
	})
	cases := []struct {
		name, value, step string
		edit              func(map[string]fake.Screen)
	}{
		{name: "sensitive input", value: account, step: "s3"},
		// An unprotected field, so only the template tells the engine the
		// value is a secret.
		{name: "secret", value: password, step: "s1", edit: func(s map[string]fake.Screen) {
			for i := range s["signon"].Elements {
				s["signon"].Elements[i].Protected = false
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" is never quoted", func(t *testing.T) {
			r := newRig(t, config{edit: tc.edit})
			truncateTyping(r.app, tc.value)
			res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
			f := wantFailure(t, res, replay.FailReadback)
			if f.Step != tc.step {
				t.Errorf("failed at %s, want %s", f.Step, tc.step)
			}
			raw, _ := json.Marshal(f)
			if landed := tc.value[:len(tc.value)-1]; strings.Contains(string(raw), landed) {
				t.Errorf("failure quotes the value: %s", raw)
			}
		})
	}
}

// ---- drift ----

func TestFallbackLocatorIsReportedAsDrift(t *testing.T) {
	art := transferArtifact(t)
	art.Steps[1].Target.Locators = append(art.Steps[1].Target.Locators,
		locate.Locator{Strategy: locate.ByOrdinal, Role: surface.RoleButton, Index: 1})
	approve(art)

	r := newRig(t, config{edit: func(s map[string]fake.Screen) { rename(s, "signon", "Sign On", "Log In") }})
	res := r.run(art, goodInputs(), unattended)
	wantStatus(t, res, replay.StatusSuccess)
	if len(res.Drift) != 1 || res.Drift[0].Step != "s2" || len(res.Drift[0].Missed) == 0 ||
		!strings.Contains(res.Drift[0].Used, "#1") {
		t.Errorf("drift %s, want s2 found by its positional fallback", dump(res.Drift))
	}
}

// ---- states and preconditions ----

func TestKnownStatesAndPreconditions(t *testing.T) {
	t.Run("undeclared business state", func(t *testing.T) {
		r := newRig(t, config{})
		showWhen(r.app, "Sign On", "denied", 0)
		res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
		f := wantFailure(t, res, replay.FailUnexpectedState)
		if !strings.Contains(f.Observed, "permission_denied") {
			t.Errorf("observed %q, want the state named", f.Observed)
		}
	})
	t.Run("fatal state", func(t *testing.T) {
		r := newRig(t, config{})
		showWhen(r.app, "Sign On", "rejected", 0)
		res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
		wantFailure(t, res, replay.FailFatalState)
		if res.Attempts != 1 {
			t.Errorf("attempts %d; a fatal state must not be retried", res.Attempts)
		}
	})
	t.Run("wrong application", func(t *testing.T) {
		r := newRig(t, config{edit: func(s map[string]fake.Screen) { rename(s, "signon", "Teller Workstation", "Other Product") }})
		res := r.run(approve(transferArtifact(t)), goodInputs(), unattended)
		wantFailure(t, res, replay.FailAppMismatch)
		if res.StepsCompleted != 0 {
			t.Errorf("steps completed %d, want 0", res.StepsCompleted)
		}
		for _, a := range r.app.Actions() {
			if a.Kind != "open" {
				t.Errorf("acted on the wrong application: %+v", a)
			}
		}
	})
	t.Run("invalid input", func(t *testing.T) {
		r := newRig(t, config{})
		res := r.run(approve(transferArtifact(t)), map[string]string{"account": account, "amount": "lots"}, unattended)
		wantFailure(t, res, replay.FailInvalidInput)
		if n := len(r.app.Actions()); n != 0 {
			t.Errorf("the application received %d action(s); want it never opened", n)
		}
	})
}

// ---- evidence ----

func TestFailureEvidenceIsRedacted(t *testing.T) {
	// Stop on the transfer screen with the account number typed into an
	// ordinary field; the snapshot must show the screen but not the value.
	r := newRig(t, config{})
	res := r.run(approve(transferArtifact(t)), goodInputs(), replay.Options{})
	f := wantFailure(t, res, replay.FailApprovalNeeded)

	raw, err := os.ReadFile(filepath.Join(res.Evidence, f.Observation))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), confirm) {
		t.Errorf("observation snapshot does not show the screen the run stopped on:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[input:account]") {
		t.Errorf("observation snapshot does not mask the typed account number:\n%s", raw)
	}
	// result.json and events.jsonl are checked for every run by the rig.
}
