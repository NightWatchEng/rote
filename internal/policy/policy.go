// Package policy decides what the automation is allowed to do. It is checked
// in the same way on the discovery path (the model proposes) and the replay
// path (the artifact dictates), so neither can do what the other could not.
package policy

import (
	"fmt"
	"net/url"
	pathpkg "path"
	"regexp"
	"slices"
	"strings"

	"github.com/NightWatchEng/rote/internal/surface"
)

// Risk classifies a step by what it can do to the system of record.
type Risk string

const (
	// RiskRead observes only.
	RiskRead Risk = "read"
	// RiskReversible changes on-screen state that leaving the screen undoes:
	// typing into a form, picking an option, navigating.
	RiskReversible Risk = "reversible"
	// RiskIrreversible commits something: posts a transaction, opens an
	// account. It cannot be retried or rolled back by the automation.
	RiskIrreversible Risk = "irreversible"
)

// Policy is the allowlist. Everything not listed is denied.
type Policy struct {
	// AllowedOrigins is set from the tenant binding (scheme://host:port).
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// AllowedPaths are exact paths, or prefixes ending in "*".
	AllowedPaths   []string `json:"allowed_paths"`
	AllowedActions []string `json:"allowed_actions"`
	// IrreversibleControls are regular expressions matched against the name
	// of a control being clicked.
	IrreversibleControls []string `json:"irreversible_controls"`
	// IrreversibleRequests are "METHOD /path" pairs. Where the surface can see
	// the network, these are held at the wire unless a step was explicitly
	// authorized — independent of what the control happened to be called.
	IrreversibleRequests []string `json:"irreversible_requests"`
}

// Violation is a denied operation. It is always a hard stop.
type Violation struct {
	Rule   string
	Detail string
}

func (v *Violation) Error() string { return fmt.Sprintf("policy violation (%s): %s", v.Rule, v.Detail) }

func (p *Policy) CheckAction(kind string) error {
	if !slices.Contains(p.AllowedActions, kind) {
		return &Violation{Rule: "allowed_actions", Detail: fmt.Sprintf("action %q is not permitted", kind)}
	}
	return nil
}

// CheckURL applies the origin and path allowlist to a location or a request.
func (p *Policy) CheckURL(raw string) error {
	if raw == "" || raw == "about:blank" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return &Violation{Rule: "allowed_origins", Detail: "unparseable URL"}
	}
	origin := u.Scheme + "://" + u.Host
	if !slices.Contains(p.AllowedOrigins, origin) {
		return &Violation{Rule: "allowed_origins", Detail: fmt.Sprintf("origin %s is not permitted", origin)}
	}
	// Compare the cleaned path, so "/subaccount/../x" is judged as "/x".
	path := pathpkg.Clean("/" + u.Path)
	for _, allowed := range p.AllowedPaths {
		if prefix, ok := strings.CutSuffix(allowed, "*"); ok {
			if strings.HasPrefix(path, prefix) {
				return nil
			}
		} else if path == allowed {
			return nil
		}
	}
	return &Violation{Rule: "allowed_paths", Detail: fmt.Sprintf("path %s is not permitted", path)}
}

// ClassifyControl rates an action on a control by its visible name.
func (p *Policy) ClassifyControl(kind string, el surface.Element) Risk {
	if kind == "extract" {
		return RiskRead
	}
	if kind == "click" {
		for _, pat := range p.IrreversibleControls {
			if re, err := regexp.Compile(pat); err == nil && re.MatchString(el.Name) {
				return RiskIrreversible
			}
		}
	}
	return RiskReversible
}

// IrreversibleRequest reports whether a network request commits something.
func (p *Policy) IrreversibleRequest(method, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return slices.Contains(p.IrreversibleRequests, strings.ToUpper(method)+" "+pathpkg.Clean("/"+u.Path))
}

// Validate rejects a policy that cannot be evaluated.
func (p *Policy) Validate() error {
	for _, pat := range p.IrreversibleControls {
		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("irreversible_controls: %q: %w", pat, err)
		}
	}
	return nil
}
