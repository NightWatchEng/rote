package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/rote/internal/agent"
	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/llm"
	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/redact"
	"github.com/NightWatchEng/rote/internal/runner"
	"github.com/NightWatchEng/rote/internal/surface"
	"github.com/NightWatchEng/rote/internal/surface/fake"
)

const (
	baseURL   = "http://bank.test"
	secretEnv = "FAKE_BANK_AGENT_PASSWORD"
	password  = "s3cret-pass-9"
	memberID  = "731904"
)

// Discovery runs take real settle time, so the tests run in parallel; the
// secret is therefore set once here rather than with t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(secretEnv, password)
	os.Exit(m.Run())
}

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

func link(name, href string, x, y float64) surface.Element {
	return surface.Element{Role: surface.RoleLink, Name: name, Href: href, Bounds: surface.Rect{X: x, Y: y, W: 100, H: 20}}
}

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

// member is the member summary: a redacted name, a table of accounts and a
// committing button. extra lets a variant of the same screen differ.
func member(extra ...surface.Element) fake.Screen {
	els := []surface.Element{
		text("Member Summary", 10, 10),
		text("Member #"+memberID, 300, 10),
		text("Member Name:", 10, 50), text("Jane Q Public", 200, 50),
		text("Opened:", 10, 75), text("2019-04-01", 200, 75),
		text("Account", 10, 100), text("Balance", 200, 100), text("Status", 350, 100),
		text("Regular Savings", 10, 130), text("$1,200.00", 200, 130), text("Active", 350, 130),
		text("Checking", 10, 160), text("$50.00", 200, 160), text("Active", 350, 160),
		button("Close Account", 10, 220),
	}
	return page("/member", "Member Summary", map[string]string{"Close Account": "closed"}, append(els, extra...)...)
}

func bankScreens() map[string]fake.Screen {
	return map[string]fake.Screen{
		// A legacy sign-on whose password box is an ordinary field, so the
		// secret would be on screen if nothing masked it.
		"signon": page("/signon", "Sign On", map[string]string{"Sign On": "search"},
			text("Password:", 10, 50), textbox(200, 50),
			button("Sign On", 200, 90)),
		"search": page("/search", "Member Search", map[string]string{"Search": "member", "Help": "help"},
			text("Member Search", 10, 10),
			text("Member Number:", 10, 50), textbox(200, 50),
			button("Search", 200, 90), button("Clear", 320, 90),
			link("Partner Site", "http://partner.example/offers", 10, 130),
			link("Help", baseURL+"/help", 200, 130)),
		"member":  member(),
		"member2": member(text("Refreshed", 10, 250)),
		"closed":  page("/closed", "Account Closed", nil, text("Account Closed", 10, 10)),
		"help":    page("/help", "Help", nil, text("Help Topics", 10, 10)),
	}
}

// clearAt is the centre of the no-op Clear button.
var clearAt = control.Input{Kind: "click", X: 370, Y: 102}

func profile() *capability.Profile {
	return &capability.Profile{
		ID: "teller", Version: "1", Product: "Teller Workstation",
		Fingerprint: locate.Condition{Text: "Teller Workstation", Frame: "banner"},
		Policy: policy.Policy{
			AllowedPaths:         []string{"/banner", "/signon", "/search", "/member", "/closed", "/help"},
			AllowedActions:       []string{"click", "type", "select", "extract"},
			IrreversibleControls: []string{`(?i)^(confirm|close account|submit)\b`},
		},
		States: []capability.State{
			{ID: "record_not_found", Class: capability.ClassBusiness, Description: "No record matches.",
				When: locate.Condition{Text: "No member found", Frame: "main"}},
			{ID: "permission_denied", Class: capability.ClassBusiness, Description: "Not permitted.",
				When: locate.Condition{Text: "Access denied", Frame: "main"}},
			{ID: "signon_rejected", Class: capability.ClassFatal, When: locate.Condition{Text: "Invalid operator ID", Frame: "main"}},
		},
		Redaction: capability.Redaction{FieldLabels: []string{"Member Name:"}},
	}
}

func spec(entry string, outputs ...capability.Output) *agent.Spec {
	return &agent.Spec{
		ID: "member.balance", Version: "1.0.0", Title: "Savings balance",
		Goal:    "Read the savings balance of member {{inputs.member_id}}",
		Entry:   entry,
		Inputs:  []capability.Param{{Name: "member_id", Type: "string", Pattern: `^\d+$`}},
		Secrets: []string{"password"},
		Outputs: outputs,
	}
}

var balance = capability.Output{Name: "balance", Type: "money"}

// ---- a scripted model ----

// turn produces one decision from the prompt the agent sent, so the script
// can point at refs that are only valid for that screen.
type turn func(t *testing.T, prompt string) agent.Decision

type scriptedLLM struct {
	t       *testing.T
	mu      sync.Mutex
	turns   []turn
	prompts []string
}

func (s *scriptedLLM) Backend() string { return "scripted" }
func (s *scriptedLLM) Model() string   { return "script-1" }

func (s *scriptedLLM) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, req.Prompt)
	if len(s.turns) == 0 {
		return llm.Response{}, errors.New("script exhausted")
	}
	next := s.turns[0]
	s.turns = s.turns[1:]
	raw, err := json.Marshal(next(s.t, req.Prompt))
	return llm.Response{JSON: raw, InputTokens: 10, OutputTokens: 5}, err
}

func (s *scriptedLLM) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

// screenLines is the CURRENT SCREEN part of a prompt.
func screenLines(prompt string) []string {
	_, screen, _ := strings.Cut(prompt, "\nCURRENT SCREEN\n")
	return strings.Split(screen, "\n")
}

var refRe = regexp.MustCompile(`^\[(\d+)\] `)

// refOnRow returns the ref of the nth item (0-based) on the screen row that
// contains the given quoted text.
func refOnRow(t *testing.T, prompt, rowText string, n int) int {
	t.Helper()
	for _, line := range screenLines(prompt) {
		if !strings.Contains(line, strconv.Quote(rowText)) {
			continue
		}
		items := strings.Split(line, " | ")
		if n < len(items) {
			if m := refRe.FindStringSubmatch(items[n]); m != nil {
				ref, _ := strconv.Atoi(m[1])
				return ref
			}
		}
	}
	t.Errorf("no item %d on a row with %q in:\n%s", n, rowText, prompt)
	return 0
}

// refOf returns the ref of the element with this role and name.
func refOf(t *testing.T, prompt, role, name string) int {
	t.Helper()
	re := regexp.MustCompile(`\[(\d+)\] ` + role + ` ` + regexp.QuoteMeta(strconv.Quote(name)))
	for _, line := range screenLines(prompt) {
		if m := re.FindStringSubmatch(line); m != nil {
			ref, _ := strconv.Atoi(m[1])
			return ref
		}
	}
	t.Errorf("no %s %q on screen:\n%s", role, name, prompt)
	return 0
}

func click(role, name string) turn {
	return func(t *testing.T, p string) agent.Decision {
		return agent.Decision{Action: "click", Ref: refOf(t, p, role, name), Reason: "click " + name}
	}
}

func typeBeside(label, text string) turn {
	return func(t *testing.T, p string) agent.Decision {
		return agent.Decision{Action: "type", Ref: refOnRow(t, p, label, 1), Text: text, Reason: "fill " + label}
	}
}

// extractCell reads the item at column index col of the row, naming anchors
// that may or may not identify it.
func extractCell(output, row string, col int, rowAnchor, colAnchor string) turn {
	return func(t *testing.T, p string) agent.Decision {
		return agent.Decision{Action: "extract", Output: output, Ref: refOnRow(t, p, row, col),
			RowAnchor: rowAnchor, ColumnAnchor: colAnchor, Reason: "read " + output}
	}
}

func finish(proof string) turn {
	return func(*testing.T, string) agent.Decision {
		return agent.Decision{Action: "finish", Success: true, Text: proof, Reason: "goal reached"}
	}
}

func giveUp(why string) turn {
	return func(*testing.T, string) agent.Decision {
		return agent.Decision{Action: "finish", Success: false, Reason: why}
	}
}

// badRef proposes an element that does not exist: a cheap rejected action.
func badRef() turn {
	return func(*testing.T, string) agent.Decision {
		return agent.Decision{Action: "click", Ref: 99999, Reason: "guess"}
	}
}

// ---- the rig ----

type rig struct {
	t     *testing.T
	app   *fake.App
	kit   *runner.Kit
	model *scriptedLLM
	op    *operatorLog
	out   string
}

func newRig(t *testing.T, entry string, answer answerFunc, script ...turn) *rig {
	t.Helper()
	env, err := capability.NewEnvironment(profile(), &capability.Tenant{
		ID: "lakeside", Profile: "teller", BaseURL: baseURL,
		Secrets: map[string]capability.SecretRef{"password": {Env: secretEnv}},
	})
	if err != nil {
		t.Fatal(err)
	}
	app := fake.New(baseURL, entry, bankScreens())
	red := redact.New(env.Profile.Redaction.FieldLabels)
	dir := t.TempDir()
	run, err := evidence.Start(filepath.Join(dir, "run"), "test", red, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Close() })
	guard := policy.NewGuard(&env.Policy, func(string, any) {})
	sess := control.NewSession(app, run)
	sess.OnTransfer = func(st control.State) { guard.HumanInControl(st.Holder == control.HolderHuman) }
	kit := &runner.Kit{Env: env, Session: sess, Guard: guard, Run: run, Redactor: red}

	r := &rig{t: t, app: app, kit: kit, model: &scriptedLLM{t: t, turns: script}, out: filepath.Join(dir, "artifact.json")}
	if answer != nil {
		hub := control.NewHub(sess, 5*time.Second)
		kit.Escalator = hub
		r.op = operate(t, hub, answer)
	}
	return r
}

func (r *rig) discover(s *agent.Spec, maxTurns int) *agent.Result {
	r.t.Helper()
	ag := &agent.Agent{Kit: r.kit, LLM: r.model, Opts: agent.Options{MaxTurns: maxTurns, Timeout: 30 * time.Second}}
	res := ag.Discover(context.Background(), s, map[string]string{"member_id": memberID}, r.out)
	for _, p := range r.model.sent() {
		if strings.Contains(p, password) {
			r.t.Errorf("a prompt contains the secret:\n%s", p)
		}
	}
	return res
}

// artifact loads what the run saved, through the same validation replay uses.
func (r *rig) artifact() (*capability.Artifact, string) {
	r.t.Helper()
	art, err := capability.Load(r.out)
	if err != nil {
		r.t.Fatalf("saved artifact: %v", err)
	}
	raw, _ := os.ReadFile(r.out)
	return art, string(raw)
}

func (r *rig) wantNoArtifact() {
	r.t.Helper()
	if _, err := os.Stat(r.out); err == nil {
		r.t.Error("an artifact was saved for a run that did not succeed")
	}
}

// prompt is the nth prompt (0-based) the model was sent.
func (r *rig) prompt(n int) string {
	r.t.Helper()
	sent := r.model.sent()
	if n >= len(sent) {
		r.t.Fatalf("only %d prompt(s) were sent", len(sent))
	}
	return sent[n]
}

func dump(v any) string {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return string(raw)
}

func wantSuccess(t *testing.T, res *agent.Result) {
	t.Helper()
	if res.Status != "success" {
		t.Fatalf("discovery %s: %s\n%s", res.Status, res.Reason, dump(res))
	}
}

func wantFailed(t *testing.T, res *agent.Result, reason string) {
	t.Helper()
	if res.Status != "failed" || !strings.Contains(res.Reason, reason) {
		t.Fatalf("discovery %s (%q), want failed with %q", res.Status, res.Reason, reason)
	}
}

// ---- a scripted operator ----

type reply struct {
	action string
	inputs []control.Input
	before func() // runs before the answer is given, e.g. to change the screen
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
			if rp.before != nil {
				rp.before()
			}
			if rp.action == control.Resume || rp.action == control.StepDone || len(rp.inputs) > 0 {
				if err := hub.Claim(iv.ID, "olivia"); err != nil {
					t.Errorf("claim: %v", err)
					continue
				}
				for _, in := range rp.inputs {
					if _, err := hub.Input(ctx, iv.ID, "olivia", in); err != nil {
						t.Errorf("input: %v", err)
					}
				}
			}
			if err := hub.Resolve(iv.ID, "olivia", rp.action, "scripted"); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return log
}

func always(action string) answerFunc {
	return func(int, *control.Intervention) reply { return reply{action: action} }
}

func countKind(reqs []control.Request, kind control.Kind) int {
	n := 0
	for _, q := range reqs {
		if q.Kind == kind {
			n++
		}
	}
	return n
}
