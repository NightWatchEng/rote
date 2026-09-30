package policy

import (
	"errors"
	"testing"

	"github.com/NightWatchEng/rote/internal/surface"
)

const origin = "http://127.0.0.1:8080"

func teller() *Policy {
	return &Policy{
		AllowedOrigins:       []string{origin},
		AllowedPaths:         []string{"/", "/member", "/subaccount/*"},
		AllowedActions:       []string{"click", "type", "select", "extract"},
		IrreversibleControls: []string{`(?i)^(confirm|post|submit|delete|close account|transfer)\b`},
		IrreversibleRequests: []string{"POST /subaccount/confirm"},
	}
}

// rule returns the violated rule, or "" when err is nil.
func rule(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var v *Violation
	if !errors.As(err, &v) {
		t.Fatalf("error %v is not a *Violation", err)
	}
	return v.Rule
}

func TestCheckAction(t *testing.T) {
	p := teller()
	for _, kind := range []string{"click", "type", "select", "extract"} {
		if err := p.CheckAction(kind); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
	for _, kind := range []string{"navigate", "Click", ""} {
		if got := rule(t, p.CheckAction(kind)); got != "allowed_actions" {
			t.Errorf("%q: rule %q, want allowed_actions", kind, got)
		}
	}
}

func TestCheckURL(t *testing.T) {
	cases := []struct {
		name, url, rule string
	}{
		{"exact path", origin + "/member", ""},
		{"query is not part of the path", origin + "/member?no=10042", ""},
		{"no path means root", origin, ""},
		{"root", origin + "/", ""},
		{"prefix path", origin + "/subaccount/confirm", ""},
		{"prefix needs the slash", origin + "/subaccount", "allowed_paths"},
		{"exact path is not a prefix", origin + "/member/delete", "allowed_paths"},
		{"unlisted path", origin + "/admin", "allowed_paths"},
		{"blank page", "about:blank", ""},
		{"no location yet", "", ""},
		{"other port", "http://127.0.0.1:8081/member", "allowed_origins"},
		{"other scheme", "https://127.0.0.1:8080/member", "allowed_origins"},
		{"other host", "http://evil.example/member", "allowed_origins"},
		{"userinfo does not smuggle a host", "http://127.0.0.1:8080@evil.example/member", "allowed_origins"},
		{"unparseable", "http://[::1", "allowed_origins"},
	}
	p := teller()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rule(t, p.CheckURL(c.url)); got != c.rule {
				t.Fatalf("CheckURL(%q) rule %q, want %q", c.url, got, c.rule)
			}
		})
	}
}

func TestClassifyControl(t *testing.T) {
	cases := []struct {
		kind, name string
		want       Risk
	}{
		{"extract", "Confirm", RiskRead},
		{"click", "Confirm", RiskIrreversible},
		{"click", "post transaction", RiskIrreversible},
		{"click", "Close Account", RiskIrreversible},
		{"click", "Search", RiskReversible},
		{"click", "Confirmation Details", RiskReversible}, // word boundary, not prefix
		{"click", "Cancel transfer", RiskReversible},      // anchored at the start
		{"type", "Confirm", RiskReversible},               // only clicks commit
		{"select", "Transfer", RiskReversible},
	}
	p := teller()
	for _, c := range cases {
		if got := p.ClassifyControl(c.kind, surface.Element{Role: surface.RoleButton, Name: c.name}); got != c.want {
			t.Errorf("%s %q: %s, want %s", c.kind, c.name, got, c.want)
		}
	}
}

func TestIrreversibleRequest(t *testing.T) {
	cases := []struct {
		method, url string
		want        bool
	}{
		{"POST", origin + "/subaccount/confirm", true},
		{"post", origin + "/subaccount/confirm?no=10042", true},
		{"GET", origin + "/subaccount/confirm", false},
		{"POST", origin + "/subaccount/review", false},
		{"POST", "http://[::1", false},
	}
	p := teller()
	for _, c := range cases {
		if got := p.IrreversibleRequest(c.method, c.url); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.method, c.url, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := teller().Validate(); err != nil {
		t.Fatal(err)
	}
	p := teller()
	p.IrreversibleControls = append(p.IrreversibleControls, "(unclosed")
	if err := p.Validate(); err == nil {
		t.Fatal("an uncompilable pattern must be rejected")
	}
}
