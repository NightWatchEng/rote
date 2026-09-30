package policy

import "sync"

// Guard enforces the policy at the network boundary of a surface that has
// one. It is the backstop behind the step-level checks: whatever a click
// turns out to do, a request outside the allowlist never leaves, and a
// request that commits something is held unless a step was authorized to
// make it.
type Guard struct {
	pol *Policy
	log func(event string, data any)

	mu        sync.Mutex
	armed     bool
	human     bool
	commits   int
	violation *Violation
}

func NewGuard(pol *Policy, log func(event string, data any)) *Guard {
	return &Guard{pol: pol, log: log}
}

// Request is called for every request the surface is about to make. A
// non-nil error blocks it.
func (g *Guard) Request(method, url string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.pol.CheckURL(url); err != nil {
		return g.block(method, url, err.(*Violation))
	}
	if g.pol.IrreversibleRequest(method, url) {
		switch {
		case g.human:
			// A person driving the session is their own authorization.
			g.commits++
			g.log("policy.irreversible_request", map[string]any{"method": method, "url": url, "authorized_by": "human in control"})
		case g.armed:
			g.armed = false
			g.commits++
			g.log("policy.irreversible_request", map[string]any{"method": method, "url": url, "authorized_by": "approved step"})
		default:
			return g.block(method, url, &Violation{Rule: "irreversible_requests",
				Detail: method + " to a committing route was attempted by a step that was not authorized to commit"})
		}
	}
	return nil
}

func (g *Guard) block(method, url string, v *Violation) error {
	if g.violation == nil {
		g.violation = v
	}
	g.log("policy.request_blocked", map[string]any{"method": method, "url": url, "rule": v.Rule, "detail": v.Detail})
	return v
}

// Arm authorizes exactly one committing request. It is called only after an
// irreversible step has been approved.
func (g *Guard) Arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

// Disarm withdraws an unused authorization.
func (g *Guard) Disarm() {
	g.mu.Lock()
	g.armed = false
	g.mu.Unlock()
}

// HumanInControl tells the guard who is driving.
func (g *Guard) HumanInControl(yes bool) {
	g.mu.Lock()
	g.human = yes
	g.mu.Unlock()
}

// Committed reports whether any committing request has been let through
// during this run, whoever made it. It is a fact observed at the wire, so it
// holds even when a step's label gave no hint that it commits.
func (g *Guard) Committed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commits > 0
}

// Take returns and clears the first violation seen since the last call.
func (g *Guard) Take() *Violation {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := g.violation
	g.violation = nil
	return v
}
