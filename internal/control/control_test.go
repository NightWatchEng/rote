package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/redact"
	"github.com/NightWatchEng/rote/internal/surface"
)

// fakeSurface records every call that reaches the application, so a test can
// tell a refused action (never arrives) from an allowed one.
type fakeSurface struct {
	mu    sync.Mutex
	calls []string
	obs   surface.Observation
}

func newFakeSurface() *fakeSurface {
	return &fakeSurface{obs: surface.Observation{
		Seq: 1, URL: "http://bank.test/subaccount/override",
		Frames: []surface.Frame{{Name: "main"}},
		Elements: []surface.Element{
			{Ref: 1, Frame: "main", Role: surface.RoleText, Name: "Supervisor Override Required", Bounds: surface.Rect{X: 0, Y: 0, W: 800, H: 600}},
			{Ref: 2, Frame: "main", Role: surface.RoleButton, Name: "Authorize", Bounds: surface.Rect{X: 300, Y: 100, W: 80, H: 24}},
			{Ref: 3, Frame: "main", Role: surface.RoleTextbox, Name: "prefilled-account-4471", Bounds: surface.Rect{X: 100, Y: 100, W: 150, H: 24}},
		},
	}}
}

func (f *fakeSurface) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeSurface) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSurface) Open(_ context.Context, url string) error { f.record("open %s", url); return nil }
func (f *fakeSurface) Observe(context.Context) (*surface.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := f.obs
	return &cp, nil
}
func (f *fakeSurface) Click(_ context.Context, el surface.Element) error {
	f.record("click %s", el.Name)
	return nil
}
func (f *fakeSurface) Type(_ context.Context, el surface.Element, text string) error {
	f.record("type %s %s", el.Name, text)
	return nil
}
func (f *fakeSurface) Select(_ context.Context, el surface.Element, option string) error {
	f.record("select %s %s", el.Name, option)
	return nil
}
func (f *fakeSurface) Pointer(_ context.Context, x, y float64) error {
	f.record("pointer %g,%g", x, y)
	return nil
}
func (f *fakeSurface) Keys(_ context.Context, text string) error {
	f.record("keys %s", text)
	return nil
}
func (f *fakeSurface) Press(_ context.Context, key string) error {
	f.record("press %s", key)
	return nil
}
func (f *fakeSurface) Screenshot(context.Context) ([]byte, error) {
	return []byte("\x89PNG fake"), nil
}
func (f *fakeSurface) Close() error { return nil }

func newRun(t *testing.T) *evidence.Run {
	t.Helper()
	run, err := evidence.Start(t.TempDir(), "test", redact.New(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Close() })
	return run
}

func newHub(t *testing.T, timeout time.Duration) (*Hub, *fakeSurface, *evidence.Run) {
	t.Helper()
	surf := newFakeSurface()
	run := newRun(t)
	return NewHub(NewSession(surf, run), timeout), surf, run
}

type outcome struct {
	res Resolution
	err error
}

// escalate raises a request in the background, as the agent loop would, and
// returns once it is open.
func escalate(t *testing.T, ctx context.Context, h *Hub, req Request) (string, <-chan outcome) {
	t.Helper()
	opened := make(chan string, 1)
	h.Notify = func(iv *Intervention) { opened <- iv.ID }
	done := make(chan outcome, 1)
	go func() {
		res, err := h.Escalate(ctx, req)
		done <- outcome{res, err}
	}()
	select {
	case id := <-opened:
		return id, done
	case <-time.After(5 * time.Second):
		t.Fatal("intervention was never raised")
		return "", nil
	}
}

func wait(t *testing.T, done <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-done:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("Escalate did not return")
		return outcome{}
	}
}

var button = surface.Element{Ref: 2, Frame: "main", Role: surface.RoleButton, Name: "Authorize"}

func automationActions(ctx context.Context, s *Session) []error {
	return []error{
		s.Open(ctx, "http://bank.test/"),
		s.Click(ctx, button),
		s.Type(ctx, button, "x"),
		s.Select(ctx, button, "y"),
	}
}

func assertAllRefused(t *testing.T, who string, errs []error) {
	t.Helper()
	for i, err := range errs {
		if !errors.Is(err, ErrNotInControl) {
			t.Errorf("%s action %d: err = %v, want ErrNotInControl", who, i, err)
		}
	}
}

func assertState(t *testing.T, s *Session, holder Holder, operator string, epoch int) {
	t.Helper()
	st := s.Control()
	if st.Holder != holder || st.Operator != operator || st.Epoch != epoch {
		t.Fatalf("control = %s/%q epoch %d, want %s/%q epoch %d", st.Holder, st.Operator, st.Epoch, holder, operator, epoch)
	}
}

func TestNewSessionIsHeldByAutomation(t *testing.T) {
	ctx := context.Background()
	surf := newFakeSurface()
	s := NewSession(surf, newRun(t))
	assertState(t, s, HolderAutomation, "", 1)

	for i, err := range automationActions(ctx, s) {
		if err != nil {
			t.Fatalf("automation action %d refused: %v", i, err)
		}
	}
	want := []string{"open http://bank.test/", "click Authorize", "type Authorize x", "select Authorize y"}
	if got := surf.Calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("surface calls = %q, want %q", got, want)
	}

	if _, err := s.HumanInput(ctx, "alice", Input{Kind: "click", X: 310, Y: 110}); !errors.Is(err, ErrNotInControl) {
		t.Fatalf("human input while automation holds control: err = %v, want ErrNotInControl", err)
	}
	if n := len(surf.Calls()); n != len(want) {
		t.Fatalf("refused human input reached the surface (%d calls)", n)
	}
}

func TestHandoffTransfersControl(t *testing.T) {
	ctx := context.Background()
	h, surf, _ := newHub(t, time.Minute)
	s := h.Session()
	var mu sync.Mutex
	var transfers []State
	s.OnTransfer = func(st State) {
		mu.Lock()
		transfers = append(transfers, st)
		mu.Unlock()
	}

	id, done := escalate(t, ctx, h, Request{Kind: KindNeedsHuman, Reason: "supervisor override"})

	// Pending: nobody may act.
	assertState(t, s, HolderPending, "", 2)
	assertAllRefused(t, "automation", automationActions(ctx, s))
	if _, err := h.Input(ctx, id, "alice", Input{Kind: "key", Key: "Enter"}); !errors.Is(err, ErrNotInControl) {
		t.Fatalf("human input before claim: err = %v, want ErrNotInControl", err)
	}

	// Claimed: only the claiming operator may act.
	if err := h.Claim(id, "alice"); err != nil {
		t.Fatal(err)
	}
	assertState(t, s, HolderHuman, "alice", 3)
	assertAllRefused(t, "automation", automationActions(ctx, s))
	if _, err := h.Input(ctx, id, "bob", Input{Kind: "key", Key: "Enter"}); !errors.Is(err, ErrNotInControl) {
		t.Fatalf("input by a non-claiming operator: err = %v, want ErrNotInControl", err)
	}
	if len(surf.Calls()) != 0 {
		t.Fatalf("refused actions reached the surface: %q", surf.Calls())
	}
	if _, err := h.Input(ctx, id, "alice", Input{Kind: "key", Key: "Enter"}); err != nil {
		t.Fatalf("input by the claiming operator: %v", err)
	}

	if err := h.Resolve(id, "alice", Resume, "override entered"); err != nil {
		t.Fatal(err)
	}
	o := wait(t, done)
	if o.err != nil {
		t.Fatal(o.err)
	}
	if o.res.Action != Resume || o.res.Operator != "alice" || o.res.Note != "override entered" || len(o.res.Actions) != 1 {
		t.Fatalf("resolution = %+v", o.res)
	}
	assertState(t, s, HolderAutomation, "", 4)
	if err := s.Click(ctx, button); err != nil {
		t.Fatalf("automation refused after hand-back: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, st := range transfers {
		got = append(got, fmt.Sprintf("%s:%s:%d", st.Holder, st.Operator, st.Epoch))
	}
	want := "pending_human::2 human:alice:3 automation::4"
	if strings.Join(got, " ") != want {
		t.Fatalf("OnTransfer saw %q, want %q", strings.Join(got, " "), want)
	}
}

func TestResolveRules(t *testing.T) {
	cases := []struct {
		name      string
		kind      Kind
		claimedBy string
		operator  string
		action    string
		ok        bool
	}{
		{"approve an approval", KindApproval, "", "alice", Approve, true},
		{"deny an approval", KindApproval, "", "alice", Deny, true},
		{"approve a stuck request", KindStuck, "", "alice", Approve, false},
		{"deny a needs-human request", KindNeedsHuman, "", "alice", Deny, false},
		{"approve an approval held by someone else", KindApproval, "alice", "bob", Approve, false},
		{"resume without claiming", KindStuck, "", "alice", Resume, false},
		{"step_done without claiming", KindStuck, "", "alice", StepDone, false},
		{"resume a session claimed by someone else", KindStuck, "alice", "bob", Resume, false},
		{"step_done a session claimed by someone else", KindStuck, "alice", "bob", StepDone, false},
		{"resume by the claimer", KindStuck, "alice", "alice", Resume, true},
		{"step_done by the claimer", KindNeedsHuman, "alice", "alice", StepDone, true},
		{"abort unclaimed", KindIndeterminate, "", "alice", Abort, true},
		{"abort a session claimed by someone else", KindStuck, "alice", "bob", Abort, true},
		{"unknown action", KindStuck, "alice", "alice", "retry", false},
		{"empty operator", KindStuck, "", "", Abort, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h, _, _ := newHub(t, time.Minute)
			id, done := escalate(t, ctx, h, Request{Kind: tc.kind, Reason: "test"})
			if tc.claimedBy != "" {
				if err := h.Claim(id, tc.claimedBy); err != nil {
					t.Fatal(err)
				}
			}
			err := h.Resolve(id, tc.operator, tc.action, "")
			if tc.ok {
				if err != nil {
					t.Fatalf("Resolve refused: %v", err)
				}
				if o := wait(t, done); o.err != nil || o.res.Action != tc.action || o.res.Operator != tc.operator {
					t.Fatalf("Escalate returned %+v, %v", o.res, o.err)
				}
				assertState(t, h.Session(), HolderAutomation, "", 3+btoi(tc.claimedBy != ""))
				return
			}
			if err == nil {
				t.Fatal("Resolve accepted, want refusal")
			}
			// A refusal leaves the intervention open and control unchanged.
			if h.Current() == nil {
				t.Fatal("refused resolution closed the intervention")
			}
			if err := h.Resolve(id, "cleanup", Abort, ""); err != nil {
				t.Fatalf("abort after refusal: %v", err)
			}
			wait(t, done)
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestClaimAndResolveRefusals(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newHub(t, time.Minute)
	id, done := escalate(t, ctx, h, Request{Kind: KindStuck, Reason: "test"})

	if err := h.Claim(id, ""); err == nil {
		t.Error("claim with empty operator accepted")
	}
	if err := h.Claim("iv-99", "alice"); err == nil {
		t.Error("claim of a non-open intervention accepted")
	}
	if err := h.Resolve("iv-99", "alice", Abort, ""); err == nil {
		t.Error("resolve of a non-open intervention accepted")
	}
	if _, err := h.Input(ctx, "iv-99", "alice", Input{Kind: "key", Key: "Enter"}); err == nil {
		t.Error("input to a non-open intervention accepted")
	}
	if err := h.Claim(id, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"alice", "bob"} {
		if err := h.Claim(id, op); err == nil {
			t.Errorf("second claim by %s accepted", op)
		}
	}
	assertState(t, h.Session(), HolderHuman, "alice", 3)

	if err := h.Resolve(id, "alice", Abort, ""); err != nil {
		t.Fatal(err)
	}
	wait(t, done)
	if err := h.Resolve(id, "alice", Abort, ""); err == nil {
		t.Error("resolving an already resolved intervention accepted")
	}
	if err := h.Claim(id, "alice"); err == nil {
		t.Error("claiming an already resolved intervention accepted")
	}
}

func TestHumanActionsAreRecordedWithoutTypedText(t *testing.T) {
	ctx := context.Background()
	h, surf, run := newHub(t, time.Minute)
	id, done := escalate(t, ctx, h, Request{Kind: KindNeedsHuman, Reason: "override", StepID: "s4"})
	if err := h.Claim(id, "alice"); err != nil {
		t.Fatal(err)
	}

	const secret = "Zq7-override-5521"
	inputs := []Input{
		{Kind: "click", X: 110, Y: 110}, // the textbox
		{Kind: "type", Text: secret},
		{Kind: "click", X: 310, Y: 110}, // the button
		{Kind: "key", Key: "Enter"},
	}
	for _, in := range inputs {
		if _, err := h.Input(ctx, id, "alice", in); err != nil {
			t.Fatalf("input %+v: %v", in, err)
		}
	}
	if _, err := h.Input(ctx, id, "alice", Input{Kind: "scroll"}); err == nil {
		t.Error("unknown input kind accepted")
	}
	// The text must really reach the application, just never the record.
	if !contains(surf.Calls(), "keys "+secret) {
		t.Fatalf("typed text never reached the surface: %q", surf.Calls())
	}

	if err := h.Resolve(id, "alice", StepDone, ""); err != nil {
		t.Fatal(err)
	}
	o := wait(t, done)
	if o.err != nil {
		t.Fatal(o.err)
	}
	acts := o.res.Actions
	if len(acts) != 4 {
		t.Fatalf("resolution carries %d actions, want 4: %+v", len(acts), acts)
	}
	if acts[0].Element != `textbox "" in frame main` {
		t.Errorf("click on a field recorded as %q; a field's content is not a label", acts[0].Element)
	}
	if acts[1].Kind != "type" || acts[1].TextLen != len(secret) {
		t.Errorf("type recorded as %+v, want text_len %d", acts[1], len(secret))
	}
	if acts[2].Element != `button "Authorize" in frame main` {
		t.Errorf("click on the button recorded as %q", acts[2].Element)
	}
	if acts[3].Key != "Enter" || acts[3].Operator != "alice" {
		t.Errorf("key recorded as %+v", acts[3])
	}

	log, err := os.ReadFile(filepath.Join(run.Dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, "prefilled-account-4471"} {
		if strings.Contains(string(log), leak) {
			t.Errorf("events.jsonl contains %q", leak)
		}
	}
	for _, ev := range []string{`"handoff.requested"`, `"human.action"`, `"handoff.resolved"`, `"control.transfer"`, `"text_len":17`} {
		if !strings.Contains(string(log), ev) {
			t.Errorf("events.jsonl lacks %s", ev)
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestEscalateTimesOutWhenNobodyClaims(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newHub(t, 150*time.Millisecond)

	id, done := escalate(t, ctx, h, Request{Kind: KindStuck, Reason: "nobody home"})
	o := wait(t, done)
	if !errors.Is(o.err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", o.err)
	}
	assertState(t, h.Session(), HolderAutomation, "", 3)
	if h.Current() != nil {
		t.Fatal("expired intervention still open")
	}
	if err := h.Claim(id, "alice"); err == nil {
		t.Fatal("claiming an expired intervention accepted")
	}
	if err := h.Resolve(id, "alice", Abort, ""); err == nil {
		t.Fatal("resolving an expired intervention accepted")
	}
}

// Once an operator holds the session the timeout no longer applies: pulling
// the session away from someone mid-task is worse than waiting for them.
func TestEscalateWaitsForAnOperatorWhoClaimed(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newHub(t, 150*time.Millisecond)

	id, done := escalate(t, ctx, h, Request{Kind: KindStuck, Reason: "slow human"})
	if err := h.Claim(id, "alice"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // well past the hub timeout
	assertState(t, h.Session(), HolderHuman, "alice", 3)
	if _, err := h.Input(ctx, id, "alice", Input{Kind: "key", Key: "Tab"}); err != nil {
		t.Fatalf("operator lost the session after the timeout: %v", err)
	}
	if err := h.Resolve(id, "alice", Resume, ""); err != nil {
		t.Fatal(err)
	}
	if o := wait(t, done); o.err != nil || o.res.Action != Resume || len(o.res.Actions) != 1 {
		t.Fatalf("resolution = %+v, %v", o.res, o.err)
	}
	assertState(t, h.Session(), HolderAutomation, "", 4)
}

// Actions from a handoff that was cancelled must not be attributed to the
// next one.
func TestCancelledHandoffDoesNotLeakActions(t *testing.T) {
	h, _, _ := newHub(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	id, done := escalate(t, ctx, h, Request{Kind: KindStuck, Reason: "first"})
	if err := h.Claim(id, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Input(ctx, id, "alice", Input{Kind: "key", Key: "Tab"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if o := wait(t, done); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", o.err)
	}

	id2, done2 := escalate(t, context.Background(), h, Request{Kind: KindStuck, Reason: "again"})
	if err := h.Resolve(id2, "bob", Abort, ""); err != nil {
		t.Fatal(err)
	}
	if o := wait(t, done2); o.err != nil || len(o.res.Actions) != 0 {
		t.Fatalf("second resolution = %+v, %v; want no carried-over actions", o.res, o.err)
	}
}

func TestEscalateHonorsContextCancel(t *testing.T) {
	h, _, _ := newHub(t, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	_, done := escalate(t, ctx, h, Request{Kind: KindStuck, Reason: "test"})
	cancel()
	if o := wait(t, done); !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", o.err)
	}
	assertState(t, h.Session(), HolderAutomation, "", 3)
}

func TestCurrentLifecycle(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newHub(t, time.Minute)
	if h.Current() != nil {
		t.Fatal("Current before any escalation is not nil")
	}

	req := Request{Kind: KindApproval, RunID: "r1", Capability: "open_subaccount", StepID: "s7", Reason: "irreversible"}
	id, done := escalate(t, ctx, h, req)
	cur := h.Current()
	if cur == nil || cur.ID != "iv-1" || id != "iv-1" || cur.Status != "pending" || cur.Request.StepID != "s7" || cur.Request.Kind != KindApproval {
		t.Fatalf("Current = %+v", cur)
	}
	// Current hands out a copy.
	cur.Status = "tampered"
	if h.Current().Status != "pending" {
		t.Fatal("mutating Current's result changed the hub")
	}

	if err := h.Claim(id, "alice"); err != nil {
		t.Fatal(err)
	}
	if cur := h.Current(); cur.Status != "claimed" || cur.Operator != "alice" {
		t.Fatalf("after claim Current = %+v", cur)
	}
	if err := h.Resolve(id, "alice", Approve, ""); err != nil {
		t.Fatal(err)
	}
	wait(t, done)
	if h.Current() != nil {
		t.Fatal("Current after resolve is not nil")
	}

	id2, done2 := escalate(t, ctx, h, Request{Kind: KindStuck, Reason: "again"})
	if id2 != "iv-2" {
		t.Fatalf("second intervention id = %s, want iv-2", id2)
	}
	h.Resolve(id2, "alice", Abort, "")
	wait(t, done2)
}
