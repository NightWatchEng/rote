package replay_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/redact"
	"github.com/NightWatchEng/rote/internal/replay"
	"github.com/NightWatchEng/rote/internal/runner"
	"github.com/NightWatchEng/rote/internal/surface"
	"github.com/NightWatchEng/rote/internal/surface/fake"
)

const (
	baseURL   = "http://bank.test"
	secretEnv = "FAKE_BANK_OPERATOR_PASSWORD"
	password  = "s3cret-pass-9"
	account   = "00123456"
	amount    = "250.00"
)

// ---- the application ----

func text(name string, x, y float64) surface.Element {
	return surface.Element{Role: surface.RoleText, Name: name, Bounds: surface.Rect{X: x, Y: y, W: 8 * float64(len(name)), H: 20}}
}

func textbox(x, y float64) surface.Element {
	return surface.Element{Role: surface.RoleTextbox, Bounds: surface.Rect{X: x, Y: y, W: 150, H: 20}}
}

func button(name string, x, y float64) surface.Element {
	return surface.Element{Role: surface.RoleButton, Name: name, Bounds: surface.Rect{X: x, Y: y, W: 100, H: 24}}
}

// page is one screen of the teller application: a banner frame that
// identifies the product, and the main frame.
func page(path, title string, next map[string]string, els ...surface.Element) fake.Screen {
	banner := text("Teller Workstation", 0, 0)
	banner.Frame = "banner"
	for i := range els {
		els[i].Frame = "main"
	}
	return fake.Screen{
		Frames:   []surface.Frame{{Name: "banner", URL: "/banner", Title: "Banner"}, {Name: "main", URL: path, Title: title}},
		Elements: append([]surface.Element{banner}, els...),
		Next:     next,
	}
}

func bankScreens() map[string]fake.Screen {
	password := textbox(200, 50)
	password.Protected = true
	return map[string]fake.Screen{
		"signon": page("/signon", "Sign On", map[string]string{"Sign On": "transfer"},
			text("Password:", 10, 50), password,
			button("Sign On", 200, 90)),
		"transfer": page("/transfer", "Transfer", map[string]string{"Confirm Transfer": "done", "Details": "notfound"},
			text("Account:", 10, 50), textbox(200, 50),
			text("Amount:", 10, 90), textbox(200, 90),
			text("Available:", 10, 130), text("$1,200.00", 200, 130),
			button("Details", 200, 170),
			button("Confirm Transfer", 200, 210)),
		"done": page("/done", "Transfer Complete", nil,
			text("Transfer Complete", 10, 10),
			text("Confirmation No:", 10, 50), text("TX-12345", 200, 50)),
		"notice":   page("/notice", "Notice", map[string]string{"OK": "transfer"}, text("System Notice", 10, 10), button("OK", 200, 50)),
		"loading":  page("/loading", "Please wait", nil, text("Loading...", 10, 10)),
		"error":    page("/error", "Error", nil, text("Application Error", 10, 10)),
		"notfound": page("/details", "Details", nil, text("No member found", 10, 10)),
		"denied":   page("/denied", "Denied", nil, text("Access denied", 10, 10)),
		"rejected": page("/signon", "Sign On", nil, text("Invalid operator ID or password", 10, 10)),
	}
}

// confirmAt is the centre of the Confirm Transfer button, where an operator
// clicks to commit by hand.
var confirmAt = control.Input{Kind: "click", X: 250, Y: 222}

// rename changes a control's name on one screen, as a vendor upgrade would.
func rename(screens map[string]fake.Screen, screen, from, to string) {
	sc := screens[screen]
	for i := range sc.Elements {
		if sc.Elements[i].Name == from {
			sc.Elements[i].Name = to
		}
	}
	if next, ok := sc.Next[from]; ok {
		delete(sc.Next, from)
		sc.Next[to] = next
	}
}

// relabel rewrites every screen in a tenant's wording.
func relabel(screens map[string]fake.Screen, labels map[string]string) {
	for name, sc := range screens {
		for from, to := range labels {
			rename(screens, name, from, to)
		}
		for i, f := range sc.Frames {
			if to, ok := labels[f.Title]; ok {
				sc.Frames[i].Title = to
			}
		}
	}
}

// ---- product profile, tenant and artifact ----

func named(role, name string) locate.Target {
	return locate.Target{Frame: "main", Locators: []locate.Locator{{Strategy: locate.ByName, Role: role, Name: name}}}
}

func labelled(role, label string) locate.Target {
	return locate.Target{Frame: "main", Locators: []locate.Locator{{Strategy: locate.ByLabel, Role: role, Label: label, Side: "left"}}}
}

func profile() *capability.Profile {
	button := func(name string) *locate.Target { t := named(surface.RoleButton, name); return &t }
	return &capability.Profile{
		ID: "teller", Version: "1", Product: "Teller Workstation",
		Fingerprint: locate.Condition{Text: "Teller Workstation", Frame: "banner"},
		Policy: policy.Policy{
			AllowedPaths:         []string{"/*"},
			AllowedActions:       []string{"click", "type", "select", "extract"},
			IrreversibleControls: []string{`(?i)^(confirm|submit|post)\b`},
			IrreversibleRequests: []string{"POST /transfer/confirm"},
		},
		States: []capability.State{
			{ID: "system_notice", Class: capability.ClassRecoverable, When: locate.Condition{Text: "System Notice", Frame: "main"},
				Recover: &capability.Recovery{Action: capability.RecoverDismiss, Target: button("OK"), MaxAttempts: 3}},
			{ID: "app_error", Class: capability.ClassRecoverable, When: locate.Condition{Text: "Application Error", Frame: "main"},
				Recover: &capability.Recovery{Action: capability.RecoverRestart, MaxAttempts: 1}},
			{ID: "signon_rejected", Class: capability.ClassFatal, Description: "credentials rejected",
				When: locate.Condition{Text: "Invalid operator ID or password", Frame: "main"}},
			{ID: "record_not_found", Class: capability.ClassBusiness, When: locate.Condition{Text: "No member found", Frame: "main"}},
			{ID: "permission_denied", Class: capability.ClassBusiness, When: locate.Condition{Text: "Access denied", Frame: "main"}},
		},
	}
}

func tenant(labels map[string]string) *capability.Tenant {
	return &capability.Tenant{
		ID: "lakeside", Profile: "teller", BaseURL: baseURL,
		Secrets: map[string]capability.SecretRef{"password": {Env: secretEnv}},
		Labels:  labels,
	}
}

func step(id, action string, target locate.Target, risk policy.Risk) capability.Step {
	return capability.Step{ID: id, Intent: "step " + id, Action: action, Target: target, Risk: risk}
}

// transferArtifact signs on, fills a transfer, commits it and reads back the
// confirmation number. It is a draft; approve() makes it runnable unattended.
func transferArtifact(t *testing.T) *capability.Artifact {
	signOn := step("s2", capability.ActionClick, named(surface.RoleButton, "Sign On"), policy.RiskReversible)
	signOn.Expect = &locate.Screen{Frame: "main", Path: "/transfer"}
	pw := step("s1", capability.ActionType, labelled(surface.RoleTextbox, "Password:"), policy.RiskReversible)
	pw.Value = "{{secrets.password}}"
	acct := step("s3", capability.ActionType, labelled(surface.RoleTextbox, "Account:"), policy.RiskReversible)
	acct.Value = "{{inputs.account}}"
	amt := step("s4", capability.ActionType, labelled(surface.RoleTextbox, "Amount:"), policy.RiskReversible)
	amt.Value = "{{inputs.amount}}"
	commit := step("s5", capability.ActionClick, named(surface.RoleButton, "Confirm Transfer"), policy.RiskIrreversible)
	commit.Expect = &locate.Screen{Frame: "main", Path: "/done", Title: "Transfer Complete"}
	read := step("s6", capability.ActionExtract, labelled(surface.RoleText, "Confirmation No:"), policy.RiskRead)
	read.Output = "confirmation"

	art := &capability.Artifact{
		SchemaVersion: capability.SchemaVersion, ID: "funds.transfer", Version: "1.0.0", Title: "Transfer funds",
		App: capability.AppRef{Profile: "teller", ProfileVersion: "1", Surface: "fake"},
		Inputs: []capability.Param{
			{Name: "account", Type: "string", Pattern: `^\d{6,}$`, Sensitive: true},
			{Name: "amount", Type: "money"},
		},
		Secrets:  []string{"password"},
		Outputs:  []capability.Output{{Name: "confirmation", Type: "string"}},
		Outcomes: []capability.Outcome{{Code: "record_not_found"}},
		Entry:    "/signon",
		Steps:    []capability.Step{pw, signOn, acct, amt, commit, read},
		Success:  locate.Condition{Text: "Transfer Complete", Frame: "main"},
		Risk:     policy.RiskIrreversible,
		Approval: capability.Approval{Status: capability.StatusDraft},
	}
	mustValidate(t, art)
	return art
}

func mustValidate(t *testing.T, art *capability.Artifact) {
	t.Helper()
	if err := art.Validate(); err != nil {
		t.Fatalf("test artifact is invalid: %v", err)
	}
}

// approve marks the artifact as reviewed, exactly as `cua approve` does.
func approve(art *capability.Artifact) *capability.Artifact {
	art.Approval = capability.Approval{Status: capability.StatusApproved, By: "reviewer", Digest: art.Digest()}
	return art
}

func goodInputs() map[string]string { return map[string]string{"account": account, "amount": amount} }

// ---- the rig: one application, one session, one run ----

type config struct {
	labels  map[string]string
	edit    func(map[string]fake.Screen)
	answer  answerFunc // nil: no operator attached
	timeout time.Duration
}

type rig struct {
	t   *testing.T
	app *fake.App
	kit *runner.Kit
	op  *operatorLog
}

func newRig(t *testing.T, c config) *rig {
	t.Helper()
	t.Setenv(secretEnv, password)
	env, err := capability.NewEnvironment(profile(), tenant(c.labels))
	if err != nil {
		t.Fatal(err)
	}
	screens := bankScreens()
	if c.labels != nil {
		relabel(screens, c.labels)
	}
	if c.edit != nil {
		c.edit(screens)
	}
	app := fake.New(baseURL, "signon", screens)

	red := redact.New(env.Profile.Redaction.FieldLabels)
	run, err := evidence.Start(t.TempDir(), "test", red, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Close() })
	guard := policy.NewGuard(&env.Policy, func(string, any) {})
	sess := control.NewSession(app, run)
	sess.OnTransfer = func(st control.State) { guard.HumanInControl(st.Holder == control.HolderHuman) }
	kit := &runner.Kit{Env: env, Session: sess, Guard: guard, Run: run, Redactor: red}

	r := &rig{t: t, app: app, kit: kit}
	if c.answer != nil {
		timeout := c.timeout
		if timeout == 0 {
			timeout = 200 * time.Millisecond
		}
		hub := control.NewHub(sess, timeout)
		kit.Escalator = hub
		r.op = operate(t, hub, c.answer)
	}
	return r
}

// run executes the artifact once and checks what must hold of every run's
// evidence, whatever its status.
func (r *rig) run(art *capability.Artifact, in map[string]string, o replay.Options) *replay.Result {
	r.t.Helper()
	if o.StepTimeout == 0 {
		o.StepTimeout = 150 * time.Millisecond
	}
	if o.Poll == 0 {
		o.Poll = 2 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := (&replay.Engine{Kit: r.kit, Opts: o}).Execute(ctx, art, in)
	r.checkEvidence(res)
	return res
}

func (r *rig) checkEvidence(res *replay.Result) {
	r.t.Helper()
	for _, name := range []string{"result.json", "events.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(res.Evidence, name))
		if err != nil {
			r.t.Errorf("evidence file %s: %v", name, err)
			continue
		}
		for _, v := range []string{password, account} {
			if strings.Contains(string(raw), v) {
				r.t.Errorf("%s contains a sensitive value %q", name, v)
			}
		}
	}
	if res.Status == replay.StatusFailed && r.app.Count("open", "") > 0 {
		if res.Failure.Observation == "" {
			r.t.Errorf("failure after the app was opened has no observation snapshot")
		} else if _, err := os.Stat(filepath.Join(res.Evidence, res.Failure.Observation)); err != nil {
			r.t.Errorf("observation snapshot: %v", err)
		}
	}
}

func dump(v any) string {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return string(raw)
}

func wantStatus(t *testing.T, res *replay.Result, status replay.Status) {
	t.Helper()
	if res.Status != status {
		t.Fatalf("status %s, want %s\n%s", res.Status, status, dump(res))
	}
}

func wantFailure(t *testing.T, res *replay.Result, kind string) *replay.Failure {
	t.Helper()
	wantStatus(t, res, replay.StatusFailed)
	if res.Failure.Kind != kind {
		t.Fatalf("failure %s, want %s\n%s", res.Failure.Kind, kind, dump(res))
	}
	if res.Outputs != nil {
		t.Errorf("a failed run returned outputs %v", res.Outputs)
	}
	return res.Failure
}

// ---- a scripted operator ----

// reply is how the scripted operator answers one intervention. An empty
// action leaves it unanswered, so the hub times out.
type reply struct {
	action string
	inputs []control.Input
}

type answerFunc func(n int, iv *control.Intervention) reply

type operatorLog struct {
	mu   sync.Mutex
	reqs []control.Request
}

func (l *operatorLog) requests() []control.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]control.Request(nil), l.reqs...)
}

// operate plays the operator at the console: it watches the hub and answers
// each intervention as it is raised, taking control first whenever the
// answer asserts something about the live session.
func operate(t *testing.T, hub *control.Hub, answer answerFunc) *operatorLog {
	log := &operatorLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := ""
		for ctx.Err() == nil {
			iv := hub.Current()
			if iv == nil || iv.ID == seen || iv.Status != "pending" {
				time.Sleep(time.Millisecond)
				continue
			}
			seen = iv.ID
			log.mu.Lock()
			log.reqs = append(log.reqs, iv.Request)
			n := len(log.reqs)
			log.mu.Unlock()

			rp := answer(n, iv)
			if rp.action == "" {
				continue
			}
			if rp.action == control.Resume || rp.action == control.StepDone || len(rp.inputs) > 0 {
				if err := hub.Claim(iv.ID, "olivia"); err != nil {
					t.Errorf("claim %s: %v", iv.ID, err)
					continue
				}
				for _, in := range rp.inputs {
					if _, err := hub.Input(ctx, iv.ID, "olivia", in); err != nil {
						t.Errorf("input %s: %v", iv.ID, err)
					}
				}
			}
			if err := hub.Resolve(iv.ID, "olivia", rp.action, "scripted"); err != nil {
				t.Errorf("resolve %s: %v", iv.ID, err)
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return log
}

// always answers every intervention the same way.
func always(action string) answerFunc {
	return func(int, *control.Intervention) reply { return reply{action: action} }
}

// firstOnly answers the first intervention and leaves the rest unanswered.
func firstOnly(rp reply) answerFunc {
	return func(n int, _ *control.Intervention) reply {
		if n == 1 {
			return rp
		}
		return reply{}
	}
}
