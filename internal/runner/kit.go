// Package runner holds what the discovery agent and the replay engine share:
// looking at the session through the redactor, recognising known application
// states, saving evidence, and asking for a human. Keeping these in one place
// is what guarantees the two paths see and guard the application identically.
package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/redact"
	"github.com/NightWatchEng/rote/internal/surface"
)

// Kit is the set of collaborators one run works with.
type Kit struct {
	Env       *capability.Environment
	Session   *control.Session
	Guard     *policy.Guard
	Escalator control.Escalator // nil when no operator is attached
	Run       *evidence.Run
	Redactor  *redact.Redactor

	shots int
}

// ErrNoOperator means a human was needed and none is attached to the run.
var ErrNoOperator = errors.New("no operator is attached to this run")

// Detect returns the first known application state on screen, with the text
// that identified it.
func (k *Kit) Detect(obs *surface.Observation) (*capability.State, string) {
	for i := range k.Env.Profile.States {
		st := &k.Env.Profile.States[i]
		if ok, detail := st.When.Eval(obs); ok {
			return st, detail
		}
	}
	return nil, ""
}

// Dismiss clicks a recoverable state's dismiss control and waits for the
// screen to react.
func (k *Kit) Dismiss(ctx context.Context, st *capability.State, obs *surface.Observation) error {
	if st.Recover == nil || st.Recover.Target == nil {
		return fmt.Errorf("state %s has no dismiss target", st.ID)
	}
	m, err := locate.Resolve(*st.Recover.Target, obs, true)
	if err != nil {
		return fmt.Errorf("state %s is on screen but its dismiss control is not: %w", st.ID, err)
	}
	before := obs.Fingerprint()
	if err := k.Session.Click(ctx, m.Element); err != nil {
		return err
	}
	k.AwaitChange(ctx, before, 3*time.Second)
	return nil
}

// AwaitChange waits until the screen differs from a fingerprint, then until
// it stops changing. It returns the latest observation and whether anything
// changed at all.
func (k *Kit) AwaitChange(ctx context.Context, before string, limit time.Duration) (*surface.Observation, bool) {
	deadline := time.Now().Add(limit)
	for {
		obs, err := k.Session.Observe(ctx)
		if err == nil && obs.Fingerprint() != before {
			settled, err := surface.Settle(ctx, k.Session, 2*time.Second)
			if err == nil {
				return settled, true
			}
			return obs, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return obs, false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// Snapshot stores a redacted screenshot and a redacted observation of the
// current screen and returns their file names (empty if unavailable).
func (k *Kit) Snapshot(ctx context.Context, label string, obs *surface.Observation) (shot, obsFile string) {
	k.shots++
	base := fmt.Sprintf("%02d-%s", k.shots, label)
	if obs == nil {
		var err error
		if obs, err = k.Session.Observe(ctx); err != nil {
			return "", ""
		}
	}
	safe, masked := k.Redactor.Observation(obs)
	obsFile = k.Run.SaveJSON(base+".observation.json", safe)
	if png, err := k.Session.Screenshot(ctx); err == nil {
		shot = k.Run.Screenshot(base+".png", png, masked)
	}
	return shot, obsFile
}

// Escalate raises an intervention request, attaching the context an operator
// needs, and blocks until it is resolved.
func (k *Kit) Escalate(ctx context.Context, req control.Request, obs *surface.Observation) (control.Resolution, error) {
	if k.Escalator == nil {
		return control.Resolution{}, ErrNoOperator
	}
	req.RunID = k.Run.ID
	if obs != nil {
		safe, _ := k.Redactor.Observation(obs)
		req.Location = obs.URL
		req.Screen = ScreenLines(safe, false)
	}
	req.Screenshot, _ = k.Snapshot(ctx, "handoff-"+string(req.Kind), obs)
	before := ""
	if obs != nil {
		before = obs.Fingerprint()
	}
	res, err := k.Escalator.Escalate(ctx, req)
	if err == nil && len(res.Actions) > 0 {
		// The operator's last action may still be taking effect (a form
		// they submitted is loading). Give the screen a moment to move and
		// settle before automation judges it, or it will see the old one.
		k.AwaitChange(ctx, before, 3*time.Second)
	}
	return res, err
}

// Observed is a short, redacted description of what is on screen in a frame
// (or, if that frame is gone, of everything), for failure reports.
func (k *Kit) Observed(obs *surface.Observation, frame string) string {
	if obs == nil {
		return "no observation available"
	}
	safe, _ := k.Redactor.Observation(obs)
	_, scoped := safe.FrameByName(frame)
	var lines []string
	in := !scoped
	for _, l := range ScreenLines(safe, false) {
		if scoped && strings.HasPrefix(l, "== frame ") {
			in = strings.HasPrefix(l, "== frame "+frame+" ")
		}
		if in {
			lines = append(lines, l)
		}
	}
	if len(lines) > 16 {
		lines = append(lines[:16], "...")
	}
	return strings.Join(lines, " / ")
}

// ScreenLines renders an observation as text: one header per frame, then one
// line per visual row, in reading order. It is what the model reads, what an
// operator's request shows, and what failure reports quote.
func ScreenLines(obs *surface.Observation, refs bool) []string {
	var out []string
	for _, f := range obs.Frames {
		header := fmt.Sprintf("== frame %s", f.Name)
		if f.Title != "" {
			header += fmt.Sprintf(" — %q", f.Title)
		}
		if p := locate.PathOf(f.URL); p != "" {
			header += " (" + p + ")"
		}
		out = append(out, header+" ==")

		var row []string
		var anchor surface.Rect
		flush := func() {
			if len(row) > 0 {
				out = append(out, strings.Join(row, " | "))
				row = nil
			}
		}
		for _, e := range obs.Elements {
			if e.Frame != f.Name {
				continue
			}
			if len(row) > 0 && e.Bounds.OverlapY(anchor) < 0.5*min(e.Bounds.H, anchor.H) {
				flush()
			}
			if len(row) == 0 {
				anchor = e.Bounds
			}
			row = append(row, describe(e, refs))
		}
		flush()
	}
	return out
}

func describe(e surface.Element, refs bool) string {
	var b strings.Builder
	if refs {
		fmt.Fprintf(&b, "[%d] ", e.Ref)
	}
	fmt.Fprintf(&b, "%s %q", e.Role, e.Name)
	switch {
	case e.Protected:
		if e.Value != "" {
			b.WriteString(" (password field, filled)")
		} else {
			b.WriteString(" (password field, empty)")
		}
	case e.Role == surface.RoleTextbox || e.Role == surface.RoleSelect || e.Role == surface.RoleCheckbox || e.Role == surface.RoleRadio:
		fmt.Fprintf(&b, " value=%q", e.Value)
	}
	if len(e.Options) > 0 {
		fmt.Fprintf(&b, " options=[%s]", strings.Join(e.Options, "; "))
	}
	return b.String()
}
