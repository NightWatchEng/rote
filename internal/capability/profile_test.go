package capability

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/surface"
)

func meridian() *Profile {
	return &Profile{
		ID:          "meridian-core",
		Version:     "1",
		Fingerprint: locate.Condition{Text: "MeridianCore Teller Workstation", Frame: "banner"},
		Policy: policy.Policy{
			AllowedPaths:         []string{"/", "/member"},
			AllowedActions:       []string{"click", "type", "select", "extract"},
			IrreversibleControls: []string{`(?i)^confirm\b`},
		},
		States: []State{
			{ID: "system_notice", Class: ClassRecoverable,
				When: locate.Condition{Any: []locate.Condition{{Text: "System Notice", Frame: "main"}, {Text: "Please Note"}}},
				Recover: &Recovery{Action: RecoverDismiss, MaxAttempts: 3,
					Target: &locate.Target{Frame: "main", Locators: []locate.Locator{{Strategy: locate.ByName, Role: surface.RoleButton, Name: "OK"}}}}},
			{ID: "record_not_found", Class: ClassBusiness, When: locate.Condition{Text: "No member found", Frame: "main"}},
		},
		Redaction: Redaction{FieldLabels: []string{"Member Number:", "SSN/TIN:"}},
	}
}

func lakeshore() *Tenant {
	return &Tenant{
		ID: "lakeshore", Profile: "meridian-core", BaseURL: "http://127.0.0.1:8081",
		Secrets: map[string]SecretRef{"operator_password": {Env: "TEST_LAKESHORE_PASSWORD"}},
		Labels: map[string]string{
			"Member Number:":  "Account Holder No.:",
			"Search":          "Find",
			"Member Detail":   "Member Record",
			"System Notice":   "Notice",
			"OK":              "Continue",
			"Current Balance": "Balance Now",
		},
	}
}

func TestNewEnvironmentLocalizesProfile(t *testing.T) {
	p := meridian()
	env, err := NewEnvironment(p, lakeshore())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env.Policy.AllowedOrigins, []string{"http://127.0.0.1:8081"}) {
		t.Fatalf("policy origins %v, want only the tenant's", env.Policy.AllowedOrigins)
	}
	if env.Policy.CheckURL("http://127.0.0.1:8080/member") == nil {
		t.Fatal("another tenant's origin must not be allowed")
	}

	notice := env.Profile.States[0]
	if got := notice.When.Any[0].Text; got != "Notice" {
		t.Errorf("state condition text %q, want the tenant's wording", got)
	}
	if got := notice.When.Any[1].Text; got != "Please Note" {
		t.Errorf("unmapped text changed to %q", got)
	}
	if got := notice.Recover.Target.Locators[0].Name; got != "Continue" {
		t.Errorf("recover target %q, want the tenant's wording", got)
	}
	if got := env.Profile.Redaction.FieldLabels; !reflect.DeepEqual(got, []string{"Account Holder No.:", "SSN/TIN:"}) {
		t.Errorf("field labels %v", got)
	}
	if env.Profile.Fingerprint.Text != "MeridianCore Teller Workstation" {
		t.Errorf("fingerprint %q", env.Profile.Fingerprint.Text)
	}

	if !reflect.DeepEqual(p, meridian()) {
		t.Fatal("the shared profile was modified by one tenant's environment")
	}
}

func TestNewEnvironmentErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *Profile, tn *Tenant)
		want   string
	}{
		{"profile mismatch", func(_ *Profile, tn *Tenant) { tn.Profile = "symitar" }, `bound to profile "symitar"`},
		{"host-less base url", func(_ *Profile, tn *Tenant) { tn.BaseURL = "/teller" }, "not an absolute URL"},
		{"schemeless base url", func(_ *Profile, tn *Tenant) { tn.BaseURL = "127.0.0.1:8081" }, "not an absolute URL"},
		{"bad policy", func(p *Profile, _ *Tenant) { p.Policy.IrreversibleControls = []string{"(confirm"} }, "irreversible_controls"},
		{"recoverable without recovery", func(p *Profile, _ *Tenant) { p.States[0].Recover = nil }, `recoverable state "system_notice" has no recovery`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, tn := meridian(), lakeshore()
			c.mutate(p, tn)
			if _, err := NewEnvironment(p, tn); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, c.want)
			}
		})
	}
}

func TestLocalize(t *testing.T) {
	env, err := NewEnvironment(meridian(), lakeshore())
	if err != nil {
		t.Fatal(err)
	}
	a := lookup()
	a.Steps[3].Target.Locators = append(a.Steps[3].Target.Locators,
		locate.Locator{Strategy: locate.ByName, Role: surface.RoleButton, Name: "Search Again"})

	got, err := env.Localize(a)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct{ what, got, want string }{
		{"label locator", got.Steps[1].Target.Locators[0].Label, "Account Holder No.:"},
		{"unmapped label", got.Steps[0].Target.Locators[0].Label, "Password:"},
		{"name locator", got.Steps[2].Target.Locators[0].Name, "Find"},
		{"grid column", got.Steps[3].Target.Locators[0].Column, "Balance Now"},
		{"grid row", got.Steps[3].Target.Locators[0].Row, "Regular Savings"},
		// Labels are whole strings: a longer name containing one is not a match.
		{"partial text", got.Steps[3].Target.Locators[1].Name, "Search Again"},
		{"expect title", got.Steps[2].Expect.Title, "Member Record"},
		{"expect path", got.Steps[2].Expect.Path, "/member"},
		{"success", got.Success.Text, "Member Record"},
		{"value", got.Steps[1].Value, "{{ inputs.member_id }}"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: %q, want %q", c.what, c.got, c.want)
		}
	}

	orig := lookup()
	orig.Steps[3].Target.Locators = append(orig.Steps[3].Target.Locators,
		locate.Locator{Strategy: locate.ByName, Role: surface.RoleButton, Name: "Search Again"})
	if !reflect.DeepEqual(a, orig) {
		t.Fatal("Localize modified the stored artifact")
	}
	// The digest is of the stored artifact; localizing must not void approval.
	approve(a)
	if _, err := env.Localize(a); err != nil || !a.Approved() {
		t.Fatalf("approval lost by localizing: %v", err)
	}
}

// A screen condition is as much tenant wording as an expect title, so the
// success condition should translate it the same way.
func TestLocalizeScreenTitleInCondition(t *testing.T) {
	env, err := NewEnvironment(meridian(), lakeshore())
	if err != nil {
		t.Fatal(err)
	}
	a := lookup()
	a.Success = locate.Condition{Screen: &locate.Screen{Frame: "main", Path: "/member", Title: "Member Detail"}}
	got, err := env.Localize(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Success.Screen.Title != "Member Record" {
		t.Fatalf("screen title in a condition was not localized: %q", got.Success.Screen.Title)
	}
	if a.Success.Screen.Title != "Member Detail" {
		t.Fatal("Localize modified the stored artifact's success screen")
	}
}

func TestLocalizeRefusesOtherProfiles(t *testing.T) {
	env, err := NewEnvironment(meridian(), lakeshore())
	if err != nil {
		t.Fatal(err)
	}
	a := lookup()
	a.App.Profile = "symitar"
	if _, err := env.Localize(a); err == nil || !strings.Contains(err.Error(), `targets profile "symitar"`) {
		t.Fatalf("got %v", err)
	}
	a = lookup()
	a.App.ProfileVersion = "2"
	if _, err := env.Localize(a); err == nil || !strings.Contains(err.Error(), "re-validate") {
		t.Fatalf("got %v", err)
	}
}

func TestCanonical(t *testing.T) {
	env, err := NewEnvironment(meridian(), lakeshore())
	if err != nil {
		t.Fatal(err)
	}
	recorded := locate.Target{Frame: "main", Rationale: "why", Locators: []locate.Locator{
		{Strategy: locate.ByLabel, Role: surface.RoleTextbox, Label: "Account Holder No.:", Side: "left"},
		{Strategy: locate.ByName, Role: surface.RoleButton, Name: "find"},
		{Strategy: locate.ByOrdinal, Role: surface.RoleTextbox, Index: 1},
	}}
	got := env.CanonicalTarget(recorded)
	want := locate.Target{Frame: "main", Rationale: "why", Locators: []locate.Locator{
		{Strategy: locate.ByLabel, Role: surface.RoleTextbox, Label: "Member Number:", Side: "left"},
		{Strategy: locate.ByName, Role: surface.RoleButton, Name: "Search"},
		{Strategy: locate.ByOrdinal, Role: surface.RoleTextbox, Index: 1},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if recorded.Locators[0].Label != "Account Holder No.:" {
		t.Fatal("CanonicalTarget modified its argument")
	}

	for in, want := range map[string]string{
		"Member Record":   "Member Detail",
		"CONTINUE":        "OK",
		"Regular Savings": "Regular Savings",
		"":                "",
	} {
		if got := env.CanonicalText(in); got != want {
			t.Errorf("CanonicalText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvironmentURLsAndSecrets(t *testing.T) {
	env, err := NewEnvironment(meridian(), lakeshore())
	if err != nil {
		t.Fatal(err)
	}
	if got := env.EntryURL("/"); got != "http://127.0.0.1:8081/" {
		t.Errorf("EntryURL = %q", got)
	}

	t.Setenv("TEST_LAKESHORE_PASSWORD", "s3cr3t")
	got, err := env.Secrets([]string{"operator_password"})
	if err != nil || got["operator_password"] != "s3cr3t" {
		t.Fatalf("Secrets = %v, %v", got, err)
	}
	if _, err := env.Secrets([]string{"operator_id"}); err == nil || !strings.Contains(err.Error(), "no binding") {
		t.Errorf("unbound secret: %v", err)
	}
	t.Setenv("TEST_LAKESHORE_PASSWORD", "")
	if _, err := env.Secrets([]string{"operator_password"}); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Errorf("unset secret: %v", err)
	}
}

func realArtifact(t *testing.T) *Artifact {
	t.Helper()
	a, err := Load(filepath.Join("..", "..", "artifacts", "member.get_savings_balance.json"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLoadTenantPinecrest(t *testing.T) {
	env, err := LoadTenant(filepath.Join("..", "..", "tenants", "pinecrest.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if env.Profile.ID != "meridian-core" || !reflect.DeepEqual(env.Policy.AllowedOrigins, []string{"http://127.0.0.1:8080"}) {
		t.Fatalf("profile %s origins %v", env.Profile.ID, env.Policy.AllowedOrigins)
	}
	a := realArtifact(t)
	got, err := env.Localize(a)
	if err != nil {
		t.Fatal(err)
	}
	// The recording tenant has no overrides: localizing is the identity.
	if !reflect.DeepEqual(got, a) {
		t.Fatal("a tenant without label overrides changed the artifact")
	}
}

func TestLoadTenantLakeshore(t *testing.T) {
	env, err := LoadTenant(filepath.Join("..", "..", "tenants", "lakeshore.json"), filepath.Join("..", "..", "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env.Policy.AllowedOrigins, []string{"http://127.0.0.1:8081"}) {
		t.Fatalf("origins %v", env.Policy.AllowedOrigins)
	}
	a := realArtifact(t)
	got, err := env.Localize(a)
	if err != nil {
		t.Fatal(err)
	}
	step := func(art *Artifact, id string) Step {
		for _, s := range art.Steps {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("artifact has no step %s", id)
		return Step{}
	}
	if l := step(got, "s5").Target.Locators[0].Label; l != "Account Holder No.:" {
		t.Errorf("member number label %q", l)
	}
	if n := step(got, "s6").Target.Locators[0].Name; n != "Find" {
		t.Errorf("search button %q", n)
	}
	if s := step(got, "s4"); s.Target.Locators[0].Name != "Member Lookup" || s.Expect.Title != "Member Lookup" {
		t.Errorf("inquiry link %q, expect title %q", s.Target.Locators[0].Name, s.Expect.Title)
	}
	if l := step(a, "s5").Target.Locators[0].Label; l != "Member Number:" {
		t.Fatalf("stored artifact modified: %q", l)
	}
	// Mapping the tenant's wording back yields the recorded artifact's targets.
	if !reflect.DeepEqual(env.CanonicalTarget(step(got, "s6").Target), step(a, "s6").Target) {
		t.Error("canonical form of a localized target differs from the recording")
	}
}

func TestLoadTenantErrors(t *testing.T) {
	if _, err := LoadTenant(filepath.Join("..", "..", "tenants", "pinecrest.json"), t.TempDir()); err == nil {
		t.Error("profile missing from the given directory")
	}
	if _, err := LoadTenant(filepath.Join(t.TempDir(), "nobody.json"), ""); err == nil {
		t.Error("missing tenant file")
	}
}
