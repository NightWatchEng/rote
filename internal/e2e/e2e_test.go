// Package e2e_test replays the committed artifacts against the in-process
// simulator through real headless Chrome and checks the replay engine's
// result contract: that replay notices runtime conditions and responds to
// each deliberately, rather than timing out or guessing.
package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/replay"
	"github.com/NightWatchEng/rote/internal/surface"
)

const (
	balanceArtifact    = "member.get_savings_balance.json"
	subAccountArtifact = "member.open_sub_account.json"
)

// Where a scenario is expected to stop on a missing control, a short step
// timeout keeps the suite fast.
const shortStep = 4 * time.Second

func member(id string) map[string]string { return map[string]string{"member_id": id} }

// ---- savings balance: the read-only capability ----

func TestBalanceSuccessAcrossInputs(t *testing.T) {
	for _, tc := range []struct{ member, balance string }{
		{"10042", "5230.18"},
		{"10058", "18902.44"},
	} {
		t.Run(tc.member, func(t *testing.T) {
			r := start(t, setup{})
			res := r.run(t, loadArtifact(t, balanceArtifact), member(tc.member))
			wantStatus(t, res, replay.StatusSuccess)
			if got := res.Outputs["savings_balance"]; got != tc.balance {
				t.Errorf("savings_balance = %v, want %s", got, tc.balance)
			}
			if res.StepsCompleted != res.StepsTotal || res.Attempts != 1 {
				t.Errorf("steps %d/%d attempts %d", res.StepsCompleted, res.StepsTotal, res.Attempts)
			}
			if len(res.Recoveries)+len(res.Drift)+len(res.Handoffs) != 0 {
				t.Errorf("clean run reported recoveries/drift/handoffs: %s", dump(res))
			}
		})
	}
}

func TestBalanceBusinessOutcomes(t *testing.T) {
	for _, tc := range []struct{ member, code, mention string }{
		{"99999", "record_not_found", "99999"},
		{"10077", "permission_denied", ""},
	} {
		t.Run(tc.code, func(t *testing.T) {
			r := start(t, setup{})
			out := wantOutcome(t, r.run(t, loadArtifact(t, balanceArtifact), member(tc.member)), tc.code)
			if !strings.Contains(out.Message, tc.mention) {
				t.Errorf("outcome message %q does not mention %q", out.Message, tc.mention)
			}
		})
	}
}

func TestBalanceInvalidInputNeverTouchesTheApp(t *testing.T) {
	r := start(t, setup{})
	res := r.run(t, loadArtifact(t, balanceArtifact), member("abc"))
	wantFailure(t, res, replay.FailInvalidInput)
	if res.StepsCompleted != 0 {
		t.Errorf("steps completed = %d", res.StepsCompleted)
	}
	if n := r.requests.Load(); n != 0 {
		t.Errorf("simulator received %d requests", n)
	}
}

func TestBalanceMissingShareFailsWithRedactedEvidence(t *testing.T) {
	r := start(t, setup{stepTimeout: shortStep})
	res := r.run(t, loadArtifact(t, balanceArtifact), member("10063"))
	f := wantFailure(t, res, replay.FailTargetNotFound)
	if f.Step == "" || f.Expected == "" || f.Observed == "" {
		t.Errorf("failure lacks step/expected/observed: %+v", f)
	}
	if f.Screenshot == "" || f.Observation == "" {
		t.Fatalf("failure lacks evidence files: %+v", f)
	}
	for _, name := range []string{f.Screenshot, f.Observation} {
		if _, err := os.Stat(filepath.Join(r.RunDir, name)); err != nil {
			t.Errorf("evidence file: %v", err)
		}
	}
	pii := []string{"000-77-9031", "Priya N. Raman", operatorPassword}
	wantAbsent(t, "saved observation", r.evidence(t, f.Observation), pii...)
	wantAbsent(t, "events.jsonl", r.evidence(t, "events.jsonl"), pii...)
	wantAbsent(t, "failure.observed", f.Observed, pii...)
}

func TestBalanceRecoveries(t *testing.T) {
	// Main-frame requests of this flow, counted by the simulator's fault
	// middleware: 0 /home (sign-on), 1 /home, 2 /inquiry, 3 POST
	// /inquiry/search, 4 /member.
	for _, tc := range []struct {
		name                      string
		kind                      string
		after, count, delayMS     int
		wantStatus                replay.Status
		wantFailure               string
		wantCondition, wantAction string
		wantAttempts              int
	}{
		{name: "notice dismissed", kind: "notice", after: 2, count: 1,
			wantStatus: replay.StatusSuccess, wantCondition: "system_notice", wantAction: capability.RecoverDismiss, wantAttempts: 1},
		{name: "session expiry restarts", kind: "expire", after: 2, count: 1,
			wantStatus: replay.StatusSuccess, wantCondition: "session_expired", wantAction: capability.RecoverRestart, wantAttempts: 2},
		{name: "one error restarts", kind: "error", after: 2, count: 1,
			wantStatus: replay.StatusSuccess, wantCondition: "app_error", wantAction: capability.RecoverRestart, wantAttempts: 2},
		{name: "persistent error exhausts", kind: "error", after: 2, count: 50,
			wantStatus: replay.StatusFailed, wantFailure: replay.FailRecovery},
		{name: "slow detail page", kind: "slow", after: 4, count: 1, delayMS: 3000,
			wantStatus: replay.StatusSuccess, wantCondition: "slow_response", wantAction: "waited", wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := start(t, setup{})
			r.fault(t, tc.kind, tc.after, tc.count, tc.delayMS)
			res := r.run(t, loadArtifact(t, balanceArtifact), member("10042"))
			wantStatus(t, res, tc.wantStatus)
			if tc.wantFailure != "" {
				wantFailure(t, res, tc.wantFailure)
				return
			}
			if got := res.Outputs["savings_balance"]; got != "5230.18" {
				t.Errorf("savings_balance = %v", got)
			}
			if res.Attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", res.Attempts, tc.wantAttempts)
			}
			rs := recoveries(res, tc.wantCondition)
			if len(rs) != 1 || rs[0].Action != tc.wantAction || rs[0].AtStep == "" {
				t.Errorf("recoveries = %+v, want one %s/%s at a step", res.Recoveries, tc.wantCondition, tc.wantAction)
			}
		})
	}
}

func TestBalancePolicyViolation(t *testing.T) {
	r := start(t, setup{profile: func(p *capability.Profile) {
		p.Policy.AllowedPaths = slices.DeleteFunc(slices.Clone(p.Policy.AllowedPaths), func(s string) bool { return s == "/member" })
	}})
	f := wantFailure(t, r.run(t, loadArtifact(t, balanceArtifact), member("10042")), replay.FailPolicy)
	if f.Step == "" {
		t.Errorf("policy failure not attributed to a step: %+v", f)
	}
}

func TestBalanceWrongApplication(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><head><title>Intranet</title></head><body><h1>Staff Intranet</h1><form><input name="q"><button>Search</button></form></body></html>`)
	}))
	t.Cleanup(other.Close)
	r := start(t, setup{baseURL: other.URL, stepTimeout: shortStep})
	res := r.run(t, loadArtifact(t, balanceArtifact), member("10042"))
	wantFailure(t, res, replay.FailAppMismatch)
	if res.StepsCompleted != 0 {
		t.Errorf("steps completed = %d", res.StepsCompleted)
	}
	if n := r.requests.Load(); n != 0 {
		t.Errorf("the real application received %d requests", n)
	}
}

var lakeshoreLabels = map[string]string{
	"Member Number:": "Account Holder No.:",
	"Search":         "Find",
	"Member Inquiry": "Member Lookup",
}

func TestBalanceTenantVariant(t *testing.T) {
	t.Run("mapped", func(t *testing.T) {
		r := start(t, setup{variant: "lakeshore", labels: lakeshoreLabels})
		res := r.run(t, loadArtifact(t, balanceArtifact), member("10042"))
		wantStatus(t, res, replay.StatusSuccess)
		if got := res.Outputs["savings_balance"]; got != "5230.18" {
			t.Errorf("savings_balance = %v", got)
		}
		if len(res.Drift) != 0 {
			t.Errorf("a fully mapped tenant should not drift: %+v", res.Drift)
		}
	})

	// Without the label binding the nav item is only found by its positional
	// fallback. The engine must say so, and the screen it lands on (titled
	// "Member Lookup") must then fail the recorded checkpoint instead of the
	// run carrying on by guesswork.
	t.Run("unmapped", func(t *testing.T) {
		r := start(t, setup{variant: "lakeshore", stepTimeout: shortStep})
		res := r.run(t, loadArtifact(t, balanceArtifact), member("10042"))
		f := wantFailure(t, res, replay.FailCheckpoint)
		if len(res.Drift) != 1 || res.Drift[0].Step != f.Step {
			t.Errorf("want the failing step to be the one reported as drifted; drift %+v, failure at %q", res.Drift, f.Step)
		}
		if !strings.Contains(f.Expected, "Member Inquiry") || !strings.Contains(f.Observed, "Member Lookup") {
			t.Errorf("failure expected/observed = %q / %q", f.Expected, f.Observed)
		}
	})
}

func TestBalanceEvidenceHygiene(t *testing.T) {
	r := start(t, setup{})
	res := r.run(t, loadArtifact(t, balanceArtifact), member("10042"))
	wantStatus(t, res, replay.StatusSuccess)
	events := r.evidence(t, "events.jsonl")
	wantAbsent(t, "events.jsonl", events, operatorPassword, "5230.18", "5,230.18")
	wantAbsent(t, "result.json", r.evidence(t, "result.json"), operatorPassword, "5230.18", "5,230.18")
}

// ---- open sub-account: the committing capability ----

func subAccount(accountType, deposit string) map[string]string {
	return map[string]string{"member_id": "10042", "account_type": accountType, "nickname": "Rainy Day Fund", "opening_deposit": deposit}
}

func approveAll(t *testing.T, hub *control.Hub, action string) {
	operate(t, hub, func(_ context.Context, _ int, iv *control.Intervention) error {
		if iv.Request.Kind != control.KindApproval {
			return fmt.Errorf("unexpected %s request: %s", iv.Request.Kind, iv.Request.Reason)
		}
		return hub.Resolve(iv.ID, "op", action, "e2e")
	})
}

func TestSubAccountDraftNeedsOperator(t *testing.T) {
	r := start(t, setup{})
	wantFailure(t, r.run(t, loadArtifact(t, subAccountArtifact), subAccount("Holiday Club", "75.00")), replay.FailNotApproved)
	if n := r.requests.Load(); n != 0 {
		t.Errorf("simulator received %d requests", n)
	}
	if n := r.confirmations(t); n != 0 {
		t.Errorf("confirmations = %d", n)
	}
}

func TestSubAccountValidationError(t *testing.T) {
	// An operator is attached (so the draft may run) but nobody answers: the
	// run must end on the application's answer before anything is asked.
	r := start(t, setup{operator: true})
	res := r.run(t, loadArtifact(t, subAccountArtifact), subAccount("Share Certificate (12 mo)", "100.00"))
	out := wantOutcome(t, res, "validation_error")
	if !strings.Contains(out.Message, "Validation Error") {
		t.Errorf("outcome message = %q", out.Message)
	}
	if len(res.Handoffs) != 0 {
		t.Errorf("handoffs = %+v", res.Handoffs)
	}
	if n := r.confirmations(t); n != 0 {
		t.Errorf("confirmations = %d", n)
	}
	// The form names the member in a sentence, under no label. The profile's
	// redaction pattern is what keeps that out of the saved observation.
	files, _ := filepath.Glob(filepath.Join(r.RunDir, "*outcome-validation_error.observation.json"))
	if len(files) != 1 {
		t.Fatalf("outcome observation files = %v", files)
	}
	wantAbsent(t, "outcome observation", r.evidence(t, filepath.Base(files[0])), "Whitfield", "Dana", "Rainy Day Fund")
	wantAbsent(t, "events.jsonl", r.evidence(t, "events.jsonl"), "Whitfield", "Rainy Day Fund")
}

func TestSubAccountOperatorApproves(t *testing.T) {
	r := start(t, setup{operator: true})
	approveAll(t, r.Hub, control.Approve)
	res := r.run(t, loadArtifact(t, subAccountArtifact), subAccount("Holiday Club", "75.00"))
	wantStatus(t, res, replay.StatusSuccess)
	for _, name := range []string{"confirmation_number", "new_suffix"} {
		if v, _ := res.Outputs[name].(string); v == "" {
			t.Errorf("output %s missing: %v", name, res.Outputs)
		}
	}
	if len(res.Handoffs) != 1 || res.Handoffs[0].Kind != control.KindApproval || res.Handoffs[0].Resolution != control.Approve || res.Handoffs[0].AtStep == "" {
		t.Errorf("handoffs = %+v", res.Handoffs)
	}
	if n := r.confirmations(t); n != 1 {
		t.Errorf("confirmations = %d, want 1", n)
	}
}

func TestSubAccountOperatorDenies(t *testing.T) {
	r := start(t, setup{operator: true})
	approveAll(t, r.Hub, control.Deny)
	f := wantFailure(t, r.run(t, loadArtifact(t, subAccountArtifact), subAccount("Holiday Club", "75.00")), replay.FailDenied)
	if f.IrreversibleDispatched {
		t.Errorf("denied run reports a dispatched commit")
	}
	if n := r.confirmations(t); n != 0 {
		t.Errorf("confirmations = %d", n)
	}
}

func TestSubAccountApprovedArtifact(t *testing.T) {
	t.Run("authorized runs unattended", func(t *testing.T) {
		r := start(t, setup{authorize: true})
		res := r.run(t, approved(loadArtifact(t, subAccountArtifact)), subAccount("Holiday Club", "75.00"))
		wantStatus(t, res, replay.StatusSuccess)
		if len(res.Handoffs) != 0 {
			t.Errorf("handoffs = %+v", res.Handoffs)
		}
		if n := r.confirmations(t); n != 1 {
			t.Errorf("confirmations = %d, want 1", n)
		}
	})
	t.Run("unauthorized stops before committing", func(t *testing.T) {
		r := start(t, setup{})
		f := wantFailure(t, r.run(t, approved(loadArtifact(t, subAccountArtifact)), subAccount("Holiday Club", "75.00")), replay.FailApprovalNeeded)
		if f.Step == "" || f.IrreversibleDispatched {
			t.Errorf("failure = %+v", f)
		}
		if n := r.confirmations(t); n != 0 {
			t.Errorf("confirmations = %d", n)
		}
	})
}

func TestSubAccountSupervisorOverride(t *testing.T) {
	in := subAccount("Money Market", "12000.00")

	t.Run("operator countersigns", func(t *testing.T) {
		r := start(t, setup{operator: true})
		hub := r.Hub
		operate(t, hub, func(ctx context.Context, n int, iv *control.Intervention) error {
			if n > 0 {
				if iv.Request.Kind != control.KindApproval {
					return fmt.Errorf("unexpected %s request: %s", iv.Request.Kind, iv.Request.Reason)
				}
				return hub.Resolve(iv.ID, "sup", control.Approve, "")
			}
			if iv.Request.Kind != control.KindNeedsHuman {
				return fmt.Errorf("first request is %s, want needs_human", iv.Request.Kind)
			}
			return countersign(ctx, hub, iv.ID)
		})
		res := r.run(t, loadArtifact(t, subAccountArtifact), in)
		wantStatus(t, res, replay.StatusSuccess)
		if len(res.Handoffs) != 2 {
			t.Fatalf("handoffs = %+v", res.Handoffs)
		}
		first, second := res.Handoffs[0], res.Handoffs[1]
		if first.Kind != control.KindNeedsHuman || first.Resolution != control.Resume || len(first.Actions) != 3 {
			t.Errorf("first handoff = %+v", first)
		}
		if second.Kind != control.KindApproval || second.Resolution != control.Approve {
			t.Errorf("second handoff = %+v", second)
		}
		if n := r.confirmations(t); n != 1 {
			t.Errorf("confirmations = %d, want 1", n)
		}
		wantAbsent(t, "events.jsonl", r.evidence(t, "events.jsonl"), overrideCode, operatorPassword)
	})

	t.Run("no operator", func(t *testing.T) {
		r := start(t, setup{authorize: true})
		f := wantFailure(t, r.run(t, approved(loadArtifact(t, subAccountArtifact)), in), replay.FailNeedsHuman)
		if f.Step == "" || f.IrreversibleDispatched {
			t.Errorf("failure = %+v", f)
		}
		if n := r.confirmations(t); n != 0 {
			t.Errorf("confirmations = %d", n)
		}
	})
}

// countersign takes the session, enters the override code the automation is
// not allowed to hold, and hands back once the application has accepted it.
func countersign(ctx context.Context, hub *control.Hub, id string) error {
	if err := hub.Claim(id, "sup"); err != nil {
		return err
	}
	obs, err := hub.Session().Observe(ctx)
	if err != nil {
		return err
	}
	find := func(match func(surface.Element) bool) (x, y float64, err error) {
		for _, e := range obs.Elements {
			if e.Frame == "main" && match(e) {
				x, y = e.Bounds.Center()
				return x, y, nil
			}
		}
		return 0, 0, errors.New("control not on the override screen")
	}
	fx, fy, err := find(func(e surface.Element) bool { return e.Role == surface.RoleTextbox && e.Protected })
	if err != nil {
		return fmt.Errorf("override field: %w", err)
	}
	bx, by, err := find(func(e surface.Element) bool { return e.Role == surface.RoleButton && e.Name == "Authorize" })
	if err != nil {
		return fmt.Errorf("authorize button: %w", err)
	}
	for _, in := range []control.Input{{Kind: "click", X: fx, Y: fy}, {Kind: "type", Text: overrideCode}, {Kind: "click", X: bx, Y: by}} {
		if _, err := hub.Input(ctx, id, "sup", in); err != nil {
			return err
		}
	}
	// Resuming before the application has moved on would only show automation
	// the override screen again.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		obs, err := hub.Session().Observe(ctx)
		if err != nil {
			continue
		}
		if f, ok := obs.FrameByName("main"); ok && f.Title == "Review New Sub-Account" {
			return hub.Resolve(id, "sup", control.Resume, "countersigned")
		}
	}
	return errors.New("the application did not accept the override")
}

func TestSubAccountNoRetryAfterCommit(t *testing.T) {
	r := start(t, setup{authorize: true})
	// The review page is the last screen before the committing POST, so a
	// fault armed once it has been served fires on that POST itself.
	r.faultAfter(http.MethodGet, "/subaccount/review", "error")
	res := r.run(t, approved(loadArtifact(t, subAccountArtifact)), subAccount("Holiday Club", "75.00"))
	f := wantFailure(t, res, replay.FailIndeterminate)
	if !f.IrreversibleDispatched {
		t.Errorf("failure does not report the dispatched commit: %+v", f)
	}
	if f.Step == "" {
		t.Errorf("indeterminate failure not attributed to a step")
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d: the flow was restarted after a commit", res.Attempts)
	}
	if len(recoveries(res, "app_error")) != 0 {
		t.Errorf("the error after a commit was treated as recoverable: %+v", res.Recoveries)
	}
}
