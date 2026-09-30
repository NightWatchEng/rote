package agent_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/surface"
	"github.com/NightWatchEng/rote/internal/surface/fake"
)

// happyPath signs on, searches for the member and reads a balance.
func happyPath(t *testing.T) (*rig, *capability.Artifact, string) {
	t.Helper()
	r := newRig(t, "signon", nil,
		typeBeside("Password:", "{{secrets.password}}"),
		click(surface.RoleButton, "Sign On"),
		typeBeside("Member Number:", "{{inputs.member_id}}"),
		click(surface.RoleButton, "Search"),
		extractCell("balance", "Regular Savings", 1, "Regular Savings", "Balance"),
		finish("Member Summary"),
	)
	res := r.discover(spec("/signon", balance), 12)
	wantSuccess(t, res)
	if res.Outputs["balance"] != "1200.00" {
		t.Errorf("balance %v, want 1200.00", res.Outputs["balance"])
	}
	art, raw := r.artifact()
	return r, art, raw
}

func TestDiscoverRecordsParameterizedArtifact(t *testing.T) {
	t.Parallel()
	_, art, raw := happyPath(t)

	if len(art.Steps) != 5 {
		t.Fatalf("recorded %d steps, want 5:\n%s", len(art.Steps), raw)
	}
	for _, v := range []string{memberID, password} {
		if strings.Contains(raw, v) {
			t.Errorf("artifact contains the concrete value %q", v)
		}
	}
	if art.Steps[0].Value != "{{secrets.password}}" || art.Steps[2].Value != "{{inputs.member_id}}" {
		t.Errorf("typed values %q, %q; want placeholders", art.Steps[0].Value, art.Steps[2].Value)
	}
	if !slices.Equal(art.Secrets, []string{"password"}) {
		t.Errorf("secrets %v, want [password]", art.Secrets)
	}

	// Expect is recorded exactly on the steps that changed screens.
	wantExpect := []*locate.Screen{
		nil,
		{Frame: "main", Path: "/search", Title: "Member Search"},
		nil,
		{Frame: "main", Path: "/member", Title: "Member Summary"},
		nil,
	}
	for i, want := range wantExpect {
		got := art.Steps[i].Expect
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("step %s expect %v, want %v", art.Steps[i].ID, got, want)
		}
	}

	var codes []string
	for _, o := range art.Outcomes {
		codes = append(codes, o.Code)
	}
	if !slices.Equal(codes, []string{"record_not_found", "permission_denied"}) {
		t.Errorf("outcomes %v, want the profile's business states", codes)
	}
	// The proof text, plus the input that was visible on the goal screen.
	want := locate.Condition{All: []locate.Condition{
		{Text: "Member Summary", Frame: "main"},
		{Text: "{{inputs.member_id}}", Frame: "main"},
	}}
	if !reflect.DeepEqual(art.Success, want) {
		t.Errorf("success condition %+v", art.Success)
	}
	if art.Approval.Status != capability.StatusDraft || art.Risk != policy.RiskReversible {
		t.Errorf("approval %q risk %q, want an unapproved reversible draft", art.Approval.Status, art.Risk)
	}
}

func TestDiscoverPromptsAreRedacted(t *testing.T) {
	t.Parallel()
	r, _, _ := happyPath(t) // every prompt is also checked for the secret by the rig

	if p := r.prompt(1); !strings.Contains(p, `value="[secret:password]"`) {
		t.Errorf("the filled password field is not shown masked:\n%s", p)
	}
	memberScreen := r.prompt(4)
	for _, want := range []string{`"[REDACTED]"`, `"[AMOUNT]"`} {
		if !strings.Contains(memberScreen, want) {
			t.Errorf("member screen lacks %s:\n%s", want, memberScreen)
		}
	}
	for _, leak := range []string{"Jane Q Public", "$1,200.00"} {
		if strings.Contains(memberScreen, leak) {
			t.Errorf("member screen shows %q to the model", leak)
		}
	}
}

func TestDiscoverParameterizesTypedInputValue(t *testing.T) {
	t.Parallel()
	r := newRig(t, "search", nil,
		typeBeside("Member Number:", memberID),
		finish("Member Search"),
	)
	wantSuccess(t, r.discover(spec("/search"), 5))
	art, _ := r.artifact()
	if len(art.Steps) != 1 || art.Steps[0].Value != "{{inputs.member_id}}" {
		t.Errorf("steps %s, want the literal recorded as its placeholder", dump(art.Steps))
	}
}

func TestDiscoverRejectsUnsafeTypedText(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"undeclared placeholder":    "{{inputs.branch}}",
		"undeclared secret":         "{{secrets.api_token}}",
		"sensitive-looking literal": "123-45-6789",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, "search", nil, typeBeside("Member Number:", text), giveUp("cannot continue"))
			res := r.discover(spec("/search"), 5)
			wantFailed(t, res, "cannot continue")
			if n := r.app.Count("type", ""); n != 0 {
				t.Errorf("the text was typed %d time(s)", n)
			}
			if res.StepsRecorded != 0 {
				t.Errorf("recorded %d step(s)", res.StepsRecorded)
			}
			if p := r.prompt(1); !strings.Contains(p, "REJECTED type") {
				t.Errorf("the model was not told of the rejection:\n%s", p)
			}
			r.wantNoArtifact()
		})
	}
}

func TestDiscoverDoesNotRecordIneffectiveClick(t *testing.T) {
	t.Parallel()
	r := newRig(t, "search", nil, click(surface.RoleButton, "Clear"), giveUp("stop"))
	res := r.discover(spec("/search"), 5)
	if n := r.app.Count("click", "Clear"); n != 1 {
		t.Errorf("Clear clicked %d times, want 1", n)
	}
	if res.StepsRecorded != 0 {
		t.Errorf("recorded %d step(s) for a click that changed nothing", res.StepsRecorded)
	}
	if p := r.prompt(1); !strings.Contains(p, "nothing on screen changed") {
		t.Errorf("the model was not told the click had no effect:\n%s", p)
	}
}

func TestDiscoverRefusesNavigationOutsideAllowlist(t *testing.T) {
	t.Parallel()
	t.Run("link target", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "search", nil, click(surface.RoleLink, "Partner Site"), giveUp("stop"))
		res := r.discover(spec("/search"), 5)
		if n := r.app.Count("click", "Partner Site"); n != 0 {
			t.Errorf("off-origin link clicked %d time(s)", n)
		}
		if res.StepsRecorded != 0 {
			t.Errorf("recorded %d step(s)", res.StepsRecorded)
		}
		if p := r.prompt(1); !strings.Contains(p, "REJECTED click") || !strings.Contains(p, "not permitted") {
			t.Errorf("the model was not told why:\n%s", p)
		}
	})
	t.Run("request seen by the network guard", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "search", nil, click(surface.RoleLink, "Help"), giveUp("stop"))
		// The link itself is allowed; what the page does next is not.
		r.app.OnAction(func(a fake.Action, _ *fake.Effect) {
			if a.Kind == "click" && a.Name == "Help" {
				r.kit.Guard.Request("GET", "http://tracker.example/pixel")
			}
		})
		res := r.discover(spec("/search"), 5)
		if n := r.app.Count("click", "Help"); n != 1 {
			t.Errorf("Help clicked %d time(s), want 1", n)
		}
		if res.StepsRecorded != 0 {
			t.Errorf("recorded %d step(s) for an action policy blocked", res.StepsRecorded)
		}
		if p := r.prompt(1); !strings.Contains(p, "not permitted to go") {
			t.Errorf("the model was not told of the violation:\n%s", p)
		}
	})
}

func TestDiscoverIrreversibleClick(t *testing.T) {
	t.Parallel()
	closeAccount := click(surface.RoleButton, "Close Account")

	t.Run("no operator", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "member", nil, closeAccount, giveUp("stop"))
		res := r.discover(spec("/member"), 5)
		if n := r.app.Count("click", "Close Account"); n != 0 {
			t.Errorf("committing control clicked %d time(s) without approval", n)
		}
		if res.StepsRecorded != 0 {
			t.Errorf("recorded %d step(s)", res.StepsRecorded)
		}
		if p := r.prompt(1); !strings.Contains(p, "no operator is attached") {
			t.Errorf("the model was not told why:\n%s", p)
		}
	})

	t.Run("approved once", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "member", always(control.Approve), closeAccount, closeAccount, finish("Account Closed"))
		approvalsAtClick := -1
		r.app.OnAction(func(a fake.Action, _ *fake.Effect) {
			if a.Kind == "click" && a.Name == "Close Account" {
				approvalsAtClick = len(r.op.requests())
			}
		})
		wantSuccess(t, r.discover(spec("/member"), 6))
		if n := r.app.Count("click", "Close Account"); n != 1 {
			t.Errorf("committing control clicked %d time(s), want 1", n)
		}
		if n := countKind(r.op.requests(), control.KindApproval); n != 1 || approvalsAtClick != 1 {
			t.Errorf("%d approval(s), %d before the click; want one, given first", n, approvalsAtClick)
		}
		art, _ := r.artifact()
		if len(art.Steps) != 1 || art.Steps[0].Risk != policy.RiskIrreversible || art.Risk != policy.RiskIrreversible {
			t.Errorf("steps %s, want one irreversible step", dump(art.Steps))
		}
	})

	t.Run("approval does not survive a screen change", func(t *testing.T) {
		t.Parallel()
		var r *rig
		answer := func(n int, _ *control.Intervention) reply {
			rp := reply{action: control.Approve}
			if n == 1 {
				// While the operator is looking, the application refreshes.
				rp.before = func() { r.app.Show("member2") }
			}
			return rp
		}
		r = newRig(t, "member", answer, closeAccount, closeAccount, closeAccount, finish("Account Closed"))
		wantSuccess(t, r.discover(spec("/member"), 6))
		if n := countKind(r.op.requests(), control.KindApproval); n != 2 {
			t.Errorf("%d approval request(s), want a second one for the changed screen", n)
		}
		if n := r.app.Count("click", "Close Account"); n != 1 {
			t.Errorf("committing control clicked %d time(s), want 1", n)
		}
	})
}

func TestDiscoverExtract(t *testing.T) {
	t.Parallel()
	opened := capability.Output{Name: "opened", Type: "string"}
	r := newRig(t, "member", nil,
		// Anchors naming another row than the element chosen.
		extractCell("balance", "Regular Savings", 1, "Checking", "Balance"),
		// A status is not money.
		extractCell("balance", "Regular Savings", 2, "Regular Savings", "Status"),
		extractCell("balance", "Regular Savings", 1, "Regular Savings", "Balance"),
		extractCell("opened", "Opened:", 1, "", ""),
		finish("Member Summary"),
	)
	res := r.discover(spec("/member", balance, opened), 8)
	wantSuccess(t, res)

	if p := r.prompt(1); !strings.Contains(p, "REJECTED extract balance") || !strings.Contains(p, "not the element you chose") {
		t.Errorf("mismatched anchors not rejected:\n%s", p)
	}
	if p := r.prompt(2); !strings.Contains(p, "does not hold a money value") {
		t.Errorf("unparseable value not rejected:\n%s", p)
	}
	if res.Outputs["balance"] != "1200.00" || res.Outputs["opened"] != "2019-04-01" {
		t.Errorf("outputs %v", res.Outputs)
	}

	art, _ := r.artifact()
	if len(art.Steps) != 2 {
		t.Fatalf("recorded %s, want the two good extractions only", dump(art.Steps))
	}
	want := []locate.Locator{
		{Strategy: locate.ByGrid, Role: surface.RoleText, Row: "Regular Savings", Column: "Balance"},
		{Strategy: locate.ByLabel, Role: surface.RoleText, Label: "Opened:", Side: "left"},
	}
	for i, s := range art.Steps {
		if !slices.Equal(s.Target.Locators, want[i:i+1]) {
			t.Errorf("step %s locators %s, want %s", s.ID, dump(s.Target.Locators), dump(want[i]))
		}
	}
}

func TestDiscoverFinish(t *testing.T) {
	t.Parallel()
	t.Run("claim is verified", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "member", nil,
			finish("Member Summary"), // outputs missing
			extractCell("balance", "Regular Savings", 1, "Regular Savings", "Balance"),
			finish("Account Overview"),  // not on screen
			finish("Member #"+memberID), // data, not a heading
			finish("Member Summary"),
		)
		res := r.discover(spec("/member", balance), 8)
		wantSuccess(t, res)
		for i, want := range map[int]string{1: "have not been extracted", 3: "is not visible", 4: "not a data value"} {
			if p := r.prompt(i); !strings.Contains(p, want) {
				t.Errorf("prompt %d does not report %q:\n%s", i, want, p)
			}
		}
		if res.Turns != 5 {
			t.Errorf("turns %d, want 5", res.Turns)
		}
		art, _ := r.artifact()
		if len(art.Success.All) == 0 || art.Success.All[0].Text != "Member Summary" {
			t.Errorf("success condition %+v", art.Success)
		}
	})
	t.Run("goal reported impossible", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "member", nil, giveUp("the member record is locked"))
		res := r.discover(spec("/member", balance), 5)
		wantFailed(t, res, "the member record is locked")
		r.wantNoArtifact()
	})
}

func TestDiscoverStuck(t *testing.T) {
	t.Parallel()
	t.Run("no operator", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "search", nil, badRef(), badRef(), badRef())
		res := r.discover(spec("/search"), 10)
		wantFailed(t, res, "no progress in 3 consecutive actions")
		if !strings.Contains(res.Reason, "no operator") {
			t.Errorf("reason %q does not say no operator was attached", res.Reason)
		}
		if n := len(r.model.sent()); n != 3 {
			t.Errorf("model asked %d times, want 3", n)
		}
	})
	t.Run("operator takes over", func(t *testing.T) {
		t.Parallel()
		answer := func(int, *control.Intervention) reply {
			return reply{action: control.Resume, inputs: []control.Input{clearAt}}
		}
		r := newRig(t, "search", answer,
			badRef(), badRef(), badRef(),
			typeBeside("Member Number:", "{{inputs.member_id}}"),
			click(surface.RoleButton, "Search"),
			finish("Member Summary"),
		)
		res := r.discover(spec("/search"), 10)
		wantSuccess(t, res)
		if len(res.Handoffs) != 1 || res.Handoffs[0].Kind != control.KindStuck || res.Handoffs[0].Resolution != control.Resume {
			t.Errorf("handoffs %s, want one resumed stuck handoff", dump(res.Handoffs))
		}
		if n := r.app.Count("pointer", "Clear"); n != 1 {
			t.Errorf("operator's click reached the app %d time(s)", n)
		}
		art, _ := r.artifact()
		if !res.HumanAssisted || !art.Provenance.HumanAssisted {
			t.Error("a run a person acted in is not marked human-assisted")
		}
		if len(art.Steps) != 2 {
			t.Errorf("recorded %d steps, want 2 (the operator's click is not a step)", len(art.Steps))
		}
	})
	t.Run("turn budget", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, "search", nil, badRef(), badRef(), badRef())
		wantFailed(t, r.discover(spec("/search"), 2), "step budget of 2 turns exhausted")
	})
}
