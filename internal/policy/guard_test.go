package policy

import "testing"

type event struct {
	name string
	data map[string]any
}

func newGuard() (*Guard, *[]event) {
	var log []event
	g := NewGuard(teller(), func(name string, data any) {
		log = append(log, event{name, data.(map[string]any)})
	})
	return g, &log
}

const commit = origin + "/subaccount/confirm"

func TestGuardBlocksDisallowedURL(t *testing.T) {
	g, log := newGuard()
	if err := g.Request("GET", origin+"/member"); err != nil {
		t.Fatal(err)
	}
	if v := g.Take(); v != nil {
		t.Fatalf("allowed request left a violation: %v", v)
	}

	if err := g.Request("GET", "http://evil.example/x"); err == nil {
		t.Fatal("request to another origin was not blocked")
	}
	if err := g.Request("GET", origin+"/admin"); err == nil {
		t.Fatal("request to an unlisted path was not blocked")
	}
	// The first violation is the one reported; later ones do not overwrite it.
	v := g.Take()
	if v == nil || v.Rule != "allowed_origins" {
		t.Fatalf("Take = %v, want the allowed_origins violation", v)
	}
	if v := g.Take(); v != nil {
		t.Fatalf("Take did not clear: %v", v)
	}
	if n := len(*log); n != 2 || (*log)[0].name != "policy.request_blocked" {
		t.Fatalf("log = %v", *log)
	}
}

func TestGuardCommittingRequest(t *testing.T) {
	g, log := newGuard()

	// Not armed: held at the wire.
	if err := g.Request("POST", commit); rule(t, err) != "irreversible_requests" {
		t.Fatalf("unauthorized commit: %v", err)
	}
	if v := g.Take(); v == nil || v.Rule != "irreversible_requests" {
		t.Fatalf("Take = %v", v)
	}

	// Armed: exactly one commit goes through.
	g.Arm()
	if err := g.Request("POST", commit); err != nil {
		t.Fatalf("armed commit blocked: %v", err)
	}
	if err := g.Request("POST", commit); err == nil {
		t.Fatal("an arm authorizes one commit, not two")
	}
	g.Take()

	// Non-committing traffic does not use up the authorization.
	g.Arm()
	if err := g.Request("GET", origin+"/member"); err != nil {
		t.Fatal(err)
	}
	if err := g.Request("POST", commit); err != nil {
		t.Fatalf("authorization consumed by an unrelated request: %v", err)
	}

	// Disarm withdraws an unused authorization.
	g.Arm()
	g.Disarm()
	if err := g.Request("POST", commit); err == nil {
		t.Fatal("disarmed guard let a commit through")
	}
	g.Take()

	var authorized []any
	for _, e := range *log {
		if e.name == "policy.irreversible_request" {
			authorized = append(authorized, e.data["authorized_by"])
		}
	}
	if len(authorized) != 2 || authorized[0] != "approved step" {
		t.Fatalf("authorizations logged: %v", authorized)
	}
}

func TestGuardHumanInControl(t *testing.T) {
	g, log := newGuard()
	g.HumanInControl(true)
	for i := 0; i < 3; i++ {
		if err := g.Request("POST", commit); err != nil {
			t.Fatalf("commit %d while a human drives: %v", i, err)
		}
	}
	if last := (*log)[len(*log)-1]; last.data["authorized_by"] != "human in control" {
		t.Fatalf("log = %v", last)
	}
	// A human is still bound by the origin allowlist.
	if err := g.Request("GET", "http://evil.example/"); err == nil {
		t.Fatal("human control does not widen the allowlist")
	}
	g.Take()

	g.HumanInControl(false)
	if err := g.Request("POST", commit); err == nil {
		t.Fatal("commit allowed after the human handed back control")
	}
}
