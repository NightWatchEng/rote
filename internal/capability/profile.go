package capability

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/redact"
)

// State classes: how the replay engine must respond when a known state of
// the application is on screen.
const (
	// ClassBusiness is a legitimate answer ("no such member"). The run ends
	// and the caller is told; nothing went wrong.
	ClassBusiness = "business"
	// ClassRecoverable is an interruption the engine knows how to get past.
	ClassRecoverable = "recoverable"
	// ClassNeedsHuman is a state only a person may resolve.
	ClassNeedsHuman = "needs_human"
	// ClassFatal is a known dead end: stop with a clear error.
	ClassFatal = "fatal"
)

// Recovery actions.
const (
	RecoverDismiss = "dismiss" // click a control, then carry on with the same step
	RecoverRestart = "restart" // start the flow again from the entry point
)

// Profile is what is known about a vendor product independent of any tenant
// or capability: how to recognise it, what is allowed inside it, and the
// exceptional states it can show. One profile serves every tenant running
// the product and every capability recorded against it.
type Profile struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Product     string `json:"product"`
	Description string `json:"description"`
	// Fingerprint must hold before the first action: it proves the session
	// is looking at the product this profile (and the artifact) describes.
	Fingerprint locate.Condition `json:"fingerprint"`
	Policy      policy.Policy    `json:"policy"`
	States      []State          `json:"states"`
	Redaction   Redaction        `json:"redaction"`
}

// State is a recognisable condition of the application and what to do in it.
type State struct {
	ID          string           `json:"id"`
	Class       string           `json:"class"`
	Description string           `json:"description"`
	When        locate.Condition `json:"when"`
	Recover     *Recovery        `json:"recover,omitempty"`
}

// Recovery says how to get past a recoverable state, and how many times to
// try before treating it as a failure.
type Recovery struct {
	Action      string         `json:"action"`
	Target      *locate.Target `json:"target,omitempty"`
	MaxAttempts int            `json:"max_attempts"`
}

// Redaction is what the product is known to display that must be masked.
type Redaction struct {
	// FieldLabels are labels whose adjacent value is sensitive.
	FieldLabels []string `json:"field_labels"`
	// Patterns catch sensitive text with no label of its own.
	Patterns []RedactionPattern `json:"patterns,omitempty"`
}

// RedactionPattern is a regular expression and its replacement.
type RedactionPattern struct {
	Pattern string `json:"pattern"`
	Replace string `json:"replace"`
}

// Tenant binds one institution to a product profile: where it lives, where
// its credentials come from, and how its configuration differs.
type Tenant struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
	BaseURL string `json:"base_url"`
	// Secrets maps a secret name used by artifacts to where its value is
	// fetched at run time. Values never appear in any file.
	Secrets map[string]SecretRef `json:"secrets"`
	// Labels maps the product's canonical label text to this tenant's
	// wording. It is the whole per-tenant override surface.
	Labels map[string]string `json:"labels,omitempty"`
}

// SecretRef points at a secret. Only environment variables are implemented;
// a vault reference would be another field here.
type SecretRef struct {
	Env string `json:"env"`
}

func loadJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// LoadTenant reads a tenant binding and the profile it names, which is
// looked up next to it in ../profiles/<id>.json unless profileDir is given.
func LoadTenant(path, profileDir string) (*Environment, error) {
	var t Tenant
	if err := loadJSON(path, &t); err != nil {
		return nil, err
	}
	if profileDir == "" {
		profileDir = filepath.Join(filepath.Dir(path), "..", "profiles")
	}
	var p Profile
	if err := loadJSON(filepath.Join(profileDir, t.Profile+".json"), &p); err != nil {
		return nil, err
	}
	return NewEnvironment(&p, &t)
}

// Environment is a profile specialised for one tenant: labels translated,
// policy scoped to the tenant's origin. Discovery and replay both run inside
// one.
type Environment struct {
	Profile *Profile
	Tenant  *Tenant
	Policy  policy.Policy
	labels  map[string]string // canonical -> tenant
	reverse map[string]string // tenant -> canonical
}

func NewEnvironment(p *Profile, t *Tenant) (*Environment, error) {
	if t.Profile != p.ID {
		return nil, fmt.Errorf("tenant %s is bound to profile %q, not %q", t.ID, t.Profile, p.ID)
	}
	u, err := url.Parse(t.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("tenant %s: base_url %q is not an absolute URL", t.ID, t.BaseURL)
	}
	if err := p.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", p.ID, err)
	}
	for _, s := range p.States {
		if s.Class == ClassRecoverable && s.Recover == nil {
			return nil, fmt.Errorf("profile %s: recoverable state %q has no recovery", p.ID, s.ID)
		}
	}
	env := &Environment{Tenant: t, labels: map[string]string{}, reverse: map[string]string{}}
	for canonical, local := range t.Labels {
		env.labels[locate.Norm(canonical)] = local
		env.reverse[locate.Norm(local)] = canonical
	}

	local := *p
	local.Fingerprint = env.localizeCondition(p.Fingerprint)
	local.States = make([]State, len(p.States))
	for i, s := range p.States {
		s.When = env.localizeCondition(s.When)
		if s.Recover != nil && s.Recover.Target != nil {
			r := *s.Recover
			tgt := relabelTarget(*r.Target, env.labels)
			r.Target = &tgt
			s.Recover = &r
		}
		local.States[i] = s
	}
	local.Redaction.FieldLabels = make([]string, len(p.Redaction.FieldLabels))
	for i, l := range p.Redaction.FieldLabels {
		local.Redaction.FieldLabels[i] = relabel(l, env.labels)
	}
	env.Profile = &local
	env.Policy = p.Policy
	env.Policy.AllowedOrigins = []string{u.Scheme + "://" + u.Host}
	return env, nil
}

// NewRedactor builds the redactor for a run in this environment from the
// profile's redaction rules.
func (e *Environment) NewRedactor() (*redact.Redactor, error) {
	red := redact.New(e.Profile.Redaction.FieldLabels)
	for _, p := range e.Profile.Redaction.Patterns {
		if err := red.AddPattern(p.Pattern, p.Replace); err != nil {
			return nil, fmt.Errorf("profile %s: redaction pattern %q: %w", e.Profile.ID, p.Pattern, err)
		}
	}
	return red, nil
}

// EntryURL resolves an artifact's entry path against the tenant.
func (e *Environment) EntryURL(entry string) string {
	u, _ := url.Parse(e.Tenant.BaseURL)
	return u.ResolveReference(&url.URL{Path: entry}).String()
}

// Secrets fetches the named secrets for this tenant.
func (e *Environment) Secrets(names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range names {
		ref, ok := e.Tenant.Secrets[name]
		if !ok {
			return nil, fmt.Errorf("tenant %s has no binding for secret %q", e.Tenant.ID, name)
		}
		v := os.Getenv(ref.Env)
		if v == "" {
			return nil, fmt.Errorf("secret %q: environment variable %s is not set", name, ref.Env)
		}
		out[name] = v
	}
	return out, nil
}

// Localize returns the artifact as it applies to this tenant: every label
// the tenant words differently is swapped in. The stored artifact is never
// modified; tenants differ by binding, not by copy.
func (e *Environment) Localize(a *Artifact) (*Artifact, error) {
	if a.App.Profile != e.Profile.ID {
		return nil, fmt.Errorf("artifact %s targets profile %q; tenant %s runs %q", a.ID, a.App.Profile, e.Tenant.ID, e.Profile.ID)
	}
	if a.App.ProfileVersion != e.Profile.Version {
		return nil, fmt.Errorf("artifact %s was recorded against profile %s v%s; the current profile is v%s — re-validate the artifact before use",
			a.ID, a.App.Profile, a.App.ProfileVersion, e.Profile.Version)
	}
	cp := *a
	cp.Steps = make([]Step, len(a.Steps))
	for i, s := range a.Steps {
		s.Target = relabelTarget(s.Target, e.labels)
		if s.Expect != nil {
			exp := *s.Expect
			exp.Title = relabel(exp.Title, e.labels)
			s.Expect = &exp
		}
		cp.Steps[i] = s
	}
	cp.Success = e.localizeCondition(a.Success)
	return &cp, nil
}

// CanonicalTarget maps a target recorded on this tenant back to the
// product's canonical labels, so an artifact recorded anywhere is portable.
func (e *Environment) CanonicalTarget(t locate.Target) locate.Target {
	return relabelTarget(t, e.reverse)
}

// CanonicalText maps one piece of tenant wording back to canonical.
func (e *Environment) CanonicalText(s string) string { return relabel(s, e.reverse) }

func (e *Environment) localizeCondition(c locate.Condition) locate.Condition {
	c.Text = relabel(c.Text, e.labels)
	if c.Screen != nil {
		sc := *c.Screen
		sc.Title = relabel(sc.Title, e.labels)
		c.Screen = &sc
	}
	all, any := c.All, c.Any
	c.All, c.Any = nil, nil
	for _, sub := range all {
		c.All = append(c.All, e.localizeCondition(sub))
	}
	for _, sub := range any {
		c.Any = append(c.Any, e.localizeCondition(sub))
	}
	return c
}

func relabel(s string, m map[string]string) string {
	if v, ok := m[locate.Norm(s)]; ok && s != "" {
		return v
	}
	return s
}

func relabelTarget(t locate.Target, m map[string]string) locate.Target {
	out := t
	out.Locators = make([]locate.Locator, len(t.Locators))
	for i, l := range t.Locators {
		l.Name, l.Label, l.Row, l.Column = relabel(l.Name, m), relabel(l.Label, m), relabel(l.Row, m), relabel(l.Column, m)
		out.Locators[i] = l
	}
	return out
}
