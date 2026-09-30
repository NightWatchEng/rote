package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/legacybank"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/replay"
	"github.com/NightWatchEng/rote/internal/runner"
	"github.com/NightWatchEng/rote/internal/surface/web"
)

// Credentials the simulator accepts. They are distinctive so that a leak into
// the evidence cannot be a coincidental match.
const (
	operatorID       = "TLR0417"
	operatorPassword = "Quartz-Lantern-8841"
	overrideCode     = "OVR-5093-KESTREL"
)

var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}()

func loadArtifact(t *testing.T, name string) *capability.Artifact {
	t.Helper()
	art, err := capability.Load(filepath.Join(repoRoot, "artifacts", name))
	if err != nil {
		t.Fatalf("loading artifact %s: %v", name, err)
	}
	return art
}

func loadProfile(t *testing.T) *capability.Profile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "profiles", "meridian-core.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p capability.Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

// approved returns an in-memory copy of art marked as reviewed and approved.
func approved(art *capability.Artifact) *capability.Artifact {
	cp := *art
	cp.Approval = capability.Approval{Status: capability.StatusApproved, By: "e2e-reviewer",
		At: time.Now().UTC().Format(time.RFC3339), Digest: cp.Digest()}
	return &cp
}

// setup describes one scenario's world. The zero value is the Pinecrest
// tenant, the unmodified profile, no operator and the default step timeout.
type setup struct {
	variant     string            // simulator variant (default pinecrest)
	labels      map[string]string // tenant label overrides
	profile     func(*capability.Profile)
	baseURL     string // point the tenant somewhere other than the simulator
	operator    bool   // attach a control.Hub as the escalator
	stepTimeout time.Duration
	authorize   bool // Opts.AuthorizeIrreversible
}

// rig is one live run: its own simulator, its own browser, its own evidence.
type rig struct {
	Engine *replay.Engine
	Hub    *control.Hub // nil unless setup.operator
	SimURL string
	RunDir string

	sim      *legacybank.Server
	requests atomic.Int64 // application requests (control plane excluded)

	mu    sync.Mutex
	hooks []hook
}

// hook injects a fault once, right after a matching request has been served,
// so the fault lands on the very next main-frame request.
type hook struct {
	method, path string
	fault        url.Values
	fired        bool
}

func start(t *testing.T, s setup) *rig {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end: drives a real browser")
	}
	if s.variant == "" {
		s.variant = "pinecrest"
	}
	r := &rig{sim: legacybank.New(legacybank.Config{Variant: legacybank.Variants[s.variant],
		OperatorID: operatorID, OperatorPassword: operatorPassword, OverrideCode: overrideCode})}
	srv := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(srv.Close)
	r.SimURL = srv.URL

	t.Setenv("E2E_OPERATOR_ID", operatorID)
	t.Setenv("E2E_OPERATOR_PASSWORD", operatorPassword)
	prof := loadProfile(t)
	if s.profile != nil {
		s.profile(prof)
	}
	tenant := &capability.Tenant{ID: "e2e-" + s.variant, Profile: prof.ID, BaseURL: srv.URL, Labels: s.labels,
		Secrets: map[string]capability.SecretRef{
			"operator_id":       {Env: "E2E_OPERATOR_ID"},
			"operator_password": {Env: "E2E_OPERATOR_PASSWORD"},
		}}
	if s.baseURL != "" {
		tenant.BaseURL = s.baseURL
	}
	env, err := capability.NewEnvironment(prof, tenant)
	if err != nil {
		t.Fatal(err)
	}

	// Mirrors cmd/cua's sessionFlags.open.
	red, err := env.NewRedactor()
	if err != nil {
		t.Fatal(err)
	}
	r.RunDir = filepath.Join(t.TempDir(), "run")
	run, err := evidence.Start(r.RunDir, "replay", red, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Close() })
	guard := policy.NewGuard(&env.Policy, func(event string, data any) { run.Log("policy", event, "", data) })
	surf, err := web.New(web.Options{Headless: true, Gate: guard.Request})
	if err != nil {
		t.Skipf("browser unavailable: %v", err)
	}
	t.Cleanup(func() { surf.Close() })
	sess := control.NewSession(surf, run)
	sess.OnTransfer = func(st control.State) { guard.HumanInControl(st.Holder == control.HolderHuman) }

	kit := &runner.Kit{Env: env, Session: sess, Guard: guard, Run: run, Redactor: red}
	if s.operator {
		r.Hub = control.NewHub(sess, 30*time.Second)
		kit.Escalator = r.Hub
	}
	r.Engine = &replay.Engine{Kit: kit, Opts: replay.Options{StepTimeout: s.stepTimeout, AuthorizeIrreversible: s.authorize}}
	return r
}

func (r *rig) serve(w http.ResponseWriter, req *http.Request) {
	if !strings.HasPrefix(req.URL.Path, "/_sim/") {
		r.requests.Add(1)
	}
	r.sim.ServeHTTP(w, req)

	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.hooks {
		h := &r.hooks[i]
		if !h.fired && req.Method == h.method && req.URL.Path == h.path {
			h.fired = true
			r.control(http.MethodPost, "/_sim/fault", h.fault)
		}
	}
}

// control calls the simulator's control plane in-process.
func (r *rig) control(method, path string, form url.Values) []byte {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	r.sim.ServeHTTP(rec, req)
	return rec.Body.Bytes()
}

// fault arms a fault now. after counts main-frame requests to let through.
func (r *rig) fault(t *testing.T, kind string, after, count, delayMS int) {
	t.Helper()
	form := url.Values{"kind": {kind}, "after": {strconv.Itoa(after)}, "count": {strconv.Itoa(count)}}
	if delayMS > 0 {
		form.Set("delay_ms", strconv.Itoa(delayMS))
	}
	if out := r.control(http.MethodPost, "/_sim/fault", form); !strings.Contains(string(out), kind) {
		t.Fatalf("injecting fault: %s", out)
	}
}

// faultAfter arms a fault once the application has served method+path.
func (r *rig) faultAfter(method, path, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, hook{method: method, path: path, fault: url.Values{"kind": {kind}, "count": {"1"}}})
}

func (r *rig) confirmations(t *testing.T) int {
	t.Helper()
	var st struct {
		Confirmations int `json:"confirmations"`
	}
	if err := json.Unmarshal(r.control(http.MethodGet, "/_sim/state", nil), &st); err != nil {
		t.Fatal(err)
	}
	return st.Confirmations
}

func (r *rig) run(t *testing.T, art *capability.Artifact, in map[string]string) *replay.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res := r.Engine.Execute(ctx, art, in)
	if t.Failed() || testing.Verbose() {
		t.Logf("result: %s", dump(res))
	}
	return res
}

func (r *rig) evidence(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.RunDir, name))
	if err != nil {
		t.Fatalf("reading evidence %s: %v", name, err)
	}
	return string(raw)
}

func dump(v any) string {
	raw, _ := json.MarshalIndent(v, "", "  ")
	return string(raw)
}

// ---- assertions ----

func wantStatus(t *testing.T, res *replay.Result, status replay.Status) {
	t.Helper()
	if res.Status != status {
		t.Fatalf("status = %s, want %s\n%s", res.Status, status, dump(res))
	}
}

func wantFailure(t *testing.T, res *replay.Result, kind string) *replay.Failure {
	t.Helper()
	wantStatus(t, res, replay.StatusFailed)
	if res.Failure == nil || res.Failure.Kind != kind {
		t.Fatalf("failure = %+v, want kind %s", res.Failure, kind)
	}
	return res.Failure
}

func wantOutcome(t *testing.T, res *replay.Result, code string) *replay.Outcome {
	t.Helper()
	wantStatus(t, res, replay.StatusOutcome)
	if res.Failure != nil {
		t.Errorf("business outcome carries a failure: %+v", res.Failure)
	}
	if res.Outcome == nil || res.Outcome.Code != code {
		t.Fatalf("outcome = %+v, want code %s", res.Outcome, code)
	}
	if res.Outcome.AtStep == "" {
		t.Errorf("outcome has no at_step")
	}
	return res.Outcome
}

func wantAbsent(t *testing.T, where, text string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s contains %q", where, s)
		}
	}
}

func recoveries(res *replay.Result, condition string) []replay.Recovery {
	var out []replay.Recovery
	for _, r := range res.Recoveries {
		if r.Condition == condition {
			out = append(out, r)
		}
	}
	return out
}

// ---- the operator ----

// operate plays the human at the console: it answers each intervention the
// hub raises with handle, in order, until the test ends. Errors are reported
// on the returned channel's close via t.Error.
func operate(t *testing.T, hub *control.Hub, handle func(ctx context.Context, n int, iv *control.Intervention) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		seen := map[string]bool{}
		for n := 0; ; {
			if iv := hub.Current(); iv != nil && !seen[iv.ID] {
				seen[iv.ID] = true
				if err := handle(ctx, n, iv); err != nil {
					t.Errorf("operator on %s (%s): %v", iv.ID, iv.Request.Kind, err)
				}
				n++
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
}
