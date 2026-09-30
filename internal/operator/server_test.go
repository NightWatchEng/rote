package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/redact"
	"github.com/NightWatchEng/rote/internal/surface"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\nlive-session")

// fakeSurface stands in for the live browser session.
type fakeSurface struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSurface) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeSurface) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSurface) Open(context.Context, string) error { return nil }
func (f *fakeSurface) Observe(context.Context) (*surface.Observation, error) {
	return &surface.Observation{Elements: []surface.Element{
		{Frame: "main", Role: surface.RoleButton, Name: "Authorize", Bounds: surface.Rect{X: 10, Y: 10, W: 80, H: 20}},
	}}, nil
}
func (f *fakeSurface) Click(context.Context, surface.Element) error          { return nil }
func (f *fakeSurface) Type(context.Context, surface.Element, string) error   { return nil }
func (f *fakeSurface) Select(context.Context, surface.Element, string) error { return nil }
func (f *fakeSurface) Pointer(_ context.Context, x, y float64) error {
	f.record(fmt.Sprintf("pointer %g,%g", x, y))
	return nil
}
func (f *fakeSurface) Keys(_ context.Context, text string) error {
	f.record("keys " + text)
	return nil
}
func (f *fakeSurface) Press(_ context.Context, key string) error {
	f.record("press " + key)
	return nil
}
func (f *fakeSurface) Screenshot(context.Context) ([]byte, error) { return pngBytes, nil }
func (f *fakeSurface) Close() error                               { return nil }

type fixture struct {
	hub  *control.Hub
	surf *fakeSurface
	base string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	run, err := evidence.Start(t.TempDir(), "test", redact.New(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Close() })
	surf := &fakeSurface{}
	hub := control.NewHub(control.NewSession(surf, run), time.Minute)
	srv, err := Listen("127.0.0.1:0", hub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return &fixture{hub: hub, surf: surf, base: strings.TrimSuffix(srv.URL(), "/")}
}

type outcome struct {
	res control.Resolution
	err error
}

func (f *fixture) escalate(t *testing.T, req control.Request) (string, <-chan outcome) {
	t.Helper()
	opened := make(chan string, 1)
	f.hub.Notify = func(iv *control.Intervention) { opened <- iv.ID }
	done := make(chan outcome, 1)
	go func() {
		res, err := f.hub.Escalate(context.Background(), req)
		done <- outcome{res, err}
	}()
	select {
	case id := <-opened:
		t.Cleanup(func() { f.hub.Resolve(id, "cleanup", control.Abort, "") })
		return id, done
	case <-time.After(5 * time.Second):
		t.Fatal("intervention was never raised")
		return "", nil
	}
}

func get(t *testing.T, url string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func post(t *testing.T, url string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	if s, ok := body.(string); ok {
		raw = []byte(s)
	} else {
		raw, _ = json.Marshal(body)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type stateReply struct {
	Control      control.State         `json:"control"`
	Intervention *control.Intervention `json:"intervention"`
}

func (f *fixture) state(t *testing.T) stateReply {
	t.Helper()
	code, _, b := get(t, f.base+"/api/state")
	if code != http.StatusOK {
		t.Fatalf("/api/state = %d %s", code, b)
	}
	var st stateReply
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("/api/state body %s: %v", b, err)
	}
	return st
}

func TestConsoleIsServed(t *testing.T) {
	f := setup(t)
	code, hdr, b := get(t, f.base+"/")
	if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %s", code, hdr.Get("Content-Type"))
	}
	if !bytes.Equal(b, consoleHTML) || !strings.Contains(string(b), "<title>Operator Console</title>") {
		t.Fatal("GET / did not serve the console page")
	}
	if code, _, _ := get(t, f.base+"/nope"); code != http.StatusNotFound {
		t.Fatalf("GET /nope = %d, want 404", code)
	}
}

func TestStateAndScreenWithoutIntervention(t *testing.T) {
	f := setup(t)
	st := f.state(t)
	if st.Control.Holder != control.HolderAutomation || st.Control.Epoch != 1 || st.Intervention != nil {
		t.Fatalf("idle state = %+v", st)
	}
	// The live view is only available while someone has been asked in.
	if code, _, _ := get(t, f.base+"/api/screen"); code != http.StatusConflict {
		t.Fatalf("/api/screen with no intervention = %d, want 409", code)
	}
}

func TestFullHandoffOverHTTP(t *testing.T) {
	f := setup(t)
	id, done := f.escalate(t, control.Request{Kind: control.KindNeedsHuman, Capability: "open_subaccount", Reason: "supervisor override"})

	st := f.state(t)
	if st.Control.Holder != control.HolderPending || st.Intervention == nil || st.Intervention.ID != id ||
		st.Intervention.Status != "pending" || st.Intervention.Request.Reason != "supervisor override" {
		t.Fatalf("pending state = %+v", st)
	}
	code, hdr, shot := get(t, f.base+"/api/screen")
	if code != http.StatusOK || hdr.Get("Content-Type") != "image/png" || hdr.Get("Cache-Control") != "no-store" || !bytes.Equal(shot, pngBytes) {
		t.Fatalf("/api/screen = %d %v %q", code, hdr, shot)
	}

	if code, b := post(t, f.base+"/api/claim", map[string]string{"id": id, "operator": "alice"}); code != http.StatusOK {
		t.Fatalf("claim = %d %s", code, b)
	}
	if st := f.state(t); st.Control.Holder != control.HolderHuman || st.Control.Operator != "alice" || st.Control.Epoch != 3 {
		t.Fatalf("claimed state = %+v", st.Control)
	}

	code, b := post(t, f.base+"/api/input", map[string]any{"id": id, "operator": "alice", "kind": "click", "x": 20, "y": 15})
	if code != http.StatusOK {
		t.Fatalf("click input = %d %s", code, b)
	}
	var act control.HumanAction
	if err := json.Unmarshal(b, &act); err != nil || act.Element != `button "Authorize" in frame main` {
		t.Fatalf("click reply %s (%v)", b, err)
	}
	if code, b := post(t, f.base+"/api/input", map[string]any{"id": id, "operator": "alice", "kind": "type", "text": "4471"}); code != http.StatusOK {
		t.Fatalf("type input = %d %s", code, b)
	}
	want := []string{"pointer 20,15", "keys 4471"}
	if got := f.surf.Calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("surface calls = %q, want %q", got, want)
	}

	if code, b := post(t, f.base+"/api/resolve", map[string]string{"id": id, "operator": "alice", "action": "step_done", "note": "entered code"}); code != http.StatusOK {
		t.Fatalf("resolve = %d %s", code, b)
	}
	var o outcome
	select {
	case o = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Escalate did not return")
	}
	if o.err != nil || o.res.Action != control.StepDone || o.res.Operator != "alice" || o.res.Note != "entered code" || len(o.res.Actions) != 2 {
		t.Fatalf("resolution = %+v, %v", o.res, o.err)
	}
	if o.res.Actions[1].TextLen != 4 {
		t.Fatalf("typed action = %+v", o.res.Actions[1])
	}
	if st := f.state(t); st.Control.Holder != control.HolderAutomation || st.Control.Epoch != 4 || st.Intervention != nil {
		t.Fatalf("state after hand-back = %+v", st)
	}
}

func TestRefusalsAreConflicts(t *testing.T) {
	f := setup(t)
	id, _ := f.escalate(t, control.Request{Kind: control.KindStuck, Reason: "lost"})

	cases := []struct {
		name, path string
		body       any
		reason     string
	}{
		{"input before claim", "/api/input", map[string]any{"id": id, "operator": "alice", "kind": "key", "key": "Enter"}, "does not hold control"},
		{"resume before claim", "/api/resolve", map[string]string{"id": id, "operator": "alice", "action": "resume"}, "first take control"},
		{"approve a stuck request", "/api/resolve", map[string]string{"id": id, "operator": "alice", "action": "approve"}, "only answers an approval"},
		{"claim without operator", "/api/claim", map[string]string{"id": id}, "operator name is required"},
		{"claim unknown id", "/api/claim", map[string]string{"id": "iv-42", "operator": "alice"}, "not open"},
	}
	for _, tc := range cases {
		code, b := post(t, f.base+tc.path, tc.body)
		if code != http.StatusConflict || !strings.Contains(string(b), tc.reason) {
			t.Errorf("%s: %d %q, want 409 mentioning %q", tc.name, code, b, tc.reason)
		}
	}

	if code, b := post(t, f.base+"/api/claim", map[string]string{"id": id, "operator": "alice"}); code != http.StatusOK {
		t.Fatalf("claim = %d %s", code, b)
	}
	if code, b := post(t, f.base+"/api/claim", map[string]string{"id": id, "operator": "bob"}); code != http.StatusConflict || !strings.Contains(string(b), "already claimed by alice") {
		t.Errorf("second claim: %d %q", code, b)
	}
	if code, b := post(t, f.base+"/api/input", map[string]any{"id": id, "operator": "bob", "kind": "key", "key": "Enter"}); code != http.StatusConflict {
		t.Errorf("input by bob: %d %q", code, b)
	}
	if len(f.surf.Calls()) != 0 {
		t.Fatalf("refused input reached the surface: %q", f.surf.Calls())
	}
}

func TestBadJSONIsBadRequest(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/api/claim", "/api/input", "/api/resolve"} {
		if code, _ := post(t, f.base+path, "{not json"); code != http.StatusBadRequest {
			t.Errorf("POST %s with bad JSON = %d, want 400", path, code)
		}
	}
	if code, _, _ := get(t, f.base+"/api/claim"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/claim = %d, want 405", code)
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.10:0"} {
		if s, err := Listen(addr, nil); err == nil {
			s.Close()
			t.Errorf("Listen(%q) accepted a non-loopback address", addr)
		}
	}
}
