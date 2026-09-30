package replay

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/runner"
	"github.com/NightWatchEng/rote/internal/surface"
)

// Options tune a replay.
type Options struct {
	// StepTimeout is how long a step waits for its control, and then for the
	// screen it expects, unless the step says otherwise.
	StepTimeout time.Duration
	Poll        time.Duration
	// MaxRestarts bounds how many times the flow may be started over.
	MaxRestarts int
	// AuthorizeIrreversible is the caller's explicit consent for this
	// invocation to commit. It only counts on an approved artifact; otherwise
	// a committing step still stops for a human.
	AuthorizeIrreversible bool
}

func (o *Options) defaults() {
	if o.StepTimeout == 0 {
		o.StepTimeout = 10 * time.Second
	}
	if o.Poll == 0 {
		o.Poll = 200 * time.Millisecond
	}
	if o.MaxRestarts == 0 {
		o.MaxRestarts = 2
	}
}

// slowThreshold is how long a wait must take before it is reported as a
// recovered "slow response" rather than passing silently.
const slowThreshold = 2500 * time.Millisecond

// Engine replays artifacts.
type Engine struct {
	*runner.Kit
	Opts Options
}

// signal is how a wait or a step tells its caller what to do next.
type signal int

const (
	sigNext     signal = iota // condition met / step complete: carry on
	sigTimeout                // the wait ran out with nothing to explain it
	sigRestart                // start the flow again from the entry point
	sigStop                   // the result is final
	sigStepDone               // a human completed the current step by hand
)

type exec struct {
	*Engine
	art     *capability.Artifact
	inputs  map[string]string
	secrets map[string]string
	res     *Result

	tries     map[string]int // recovery attempts per state
	committed bool           // an irreversible step has been dispatched
	opened    bool
	// approved is decided once, on the artifact as stored: localizing it for
	// a tenant rewrites labels and would otherwise change its digest.
	approved bool
}

// Execute runs an artifact with the given inputs. It never returns an error:
// every way a run can end is a Result.
func (e *Engine) Execute(ctx context.Context, art *capability.Artifact, inputs map[string]string) *Result {
	e.Opts.defaults()
	start := time.Now()
	res := &Result{
		Capability: art.ID, Version: art.Version, Tenant: e.Env.Tenant.ID,
		RunID: e.Run.ID, Evidence: e.Run.Dir, StepsTotal: len(art.Steps),
	}
	x := &exec{Engine: e, art: art, inputs: inputs, res: res, tries: map[string]int{}, approved: art.Approved()}

	for _, p := range art.Inputs {
		if p.Sensitive {
			e.Redactor.AddLiteral("input", p.Name, inputs[p.Name])
		}
	}
	e.Run.Log("automation", "replay.start", "", map[string]any{
		"capability": art.ID, "version": art.Version, "tenant": e.Env.Tenant.ID, "inputs": capability.MaskSensitive(art.Inputs, inputs),
		"approved": x.approved, "risk": art.Risk, "operator_attached": e.Escalator != nil,
	})

	x.run(ctx)

	res.DurationMS = time.Since(start).Milliseconds()
	e.Run.Log("automation", "replay.result", "", res)
	e.Run.SaveJSON("result.json", res)
	return res
}

func (x *exec) run(ctx context.Context) {
	if err := x.art.CheckInputs(x.inputs); err != nil {
		x.fail(ctx, nil, nil, Failure{Kind: FailInvalidInput, Message: err.Error()})
		return
	}
	local, err := x.Env.Localize(x.art)
	if err != nil {
		x.fail(ctx, nil, nil, Failure{Kind: FailConfiguration, Message: err.Error()})
		return
	}
	x.art = local
	if x.secrets, err = x.Env.Secrets(x.art.Secrets); err != nil {
		x.fail(ctx, nil, nil, Failure{Kind: FailConfiguration, Message: err.Error()})
		return
	}
	for name, v := range x.secrets {
		x.Redactor.AddLiteral("secret", name, v)
	}
	if x.art.Risk == policy.RiskIrreversible && !x.approved && x.Escalator == nil {
		x.fail(ctx, nil, nil, Failure{Kind: FailNotApproved,
			Message: "this capability commits changes and its artifact is not approved; an unapproved artifact may only run with an operator attached to approve each committing step"})
		return
	}

	for attempt := 1; ; attempt++ {
		x.res.Attempts = attempt
		if sig := x.attempt(ctx); sig != sigRestart {
			return
		}
		if attempt > x.Opts.MaxRestarts {
			x.fail(ctx, nil, nil, Failure{Kind: FailRecovery,
				Message: fmt.Sprintf("the flow was restarted %d times and still could not complete", x.Opts.MaxRestarts)})
			return
		}
	}
}

// attempt runs the flow once from the entry point.
func (x *exec) attempt(ctx context.Context) signal {
	x.res.Outputs = map[string]any{}
	x.res.StepsCompleted = 0
	x.Guard.Disarm()

	if err := x.Session.Open(ctx, x.Env.EntryURL(x.art.Entry)); err != nil {
		x.fail(ctx, nil, nil, Failure{Kind: FailSurface, Message: "could not open the entry point: " + err.Error()})
		return sigStop
	}
	x.opened = true

	// Before touching anything, prove this is the product the artifact was
	// recorded against.
	fp := x.Env.Profile.Fingerprint
	obs, sig := x.await(ctx, nil, x.Opts.StepTimeout, func(o *surface.Observation) bool {
		ok, _ := fp.Eval(o)
		return ok
	})
	if sig == sigTimeout {
		x.fail(ctx, nil, obs, Failure{Kind: FailAppMismatch, Expected: fp.String(), Observed: x.Observed(obs, "top"),
			Message: "the session is not showing the application this artifact targets"})
		return sigStop
	}
	if sig != sigNext {
		return sig
	}

	for i := range x.art.Steps {
		if sig := x.step(ctx, &x.art.Steps[i]); sig != sigNext {
			return sig
		}
		x.res.StepsCompleted = i + 1
	}

	// The final checkpoint: do not assume the last step worked.
	success, err := x.renderCondition(x.art.Success)
	if err != nil {
		x.fail(ctx, nil, nil, Failure{Kind: FailConfiguration, Message: "success condition: " + err.Error()})
		return sigStop
	}
	obs, sig = x.await(ctx, nil, x.Opts.StepTimeout, func(o *surface.Observation) bool {
		ok, _ := success.Eval(o)
		return ok
	})
	switch sig {
	case sigNext:
		// Success is a promise about the contract, so every declared output
		// must actually be there.
		for _, o := range x.art.Outputs {
			if _, ok := x.res.Outputs[o.Name]; !ok {
				x.fail(ctx, nil, obs, Failure{Kind: FailOutputMissing, Expected: fmt.Sprintf("output %q", o.Name),
					Message: "the flow completed but a declared output was never read"})
				return sigStop
			}
		}
		x.res.Status = StatusSuccess
		return sigStop
	case sigTimeout:
		kind := FailCheckpoint
		if x.committed {
			kind = FailIndeterminate
		}
		x.fail(ctx, nil, obs, Failure{Kind: kind, Expected: success.String(), Observed: x.Observed(obs, ""),
			Message: "every step ran but the success condition does not hold"})
		return sigStop
	}
	return sig
}

// await polls the session until pred holds. On every poll it first checks
// the network guard and then the profile's known states, so an exceptional
// state is recognised as itself rather than as a timeout.
func (x *exec) await(ctx context.Context, s *capability.Step, timeout time.Duration, pred func(*surface.Observation) bool) (*surface.Observation, signal) {
	start := time.Now()
	deadline := start.Add(timeout)
	clean := true // no recovery happened during this wait
	observeErrors := 0
	for {
		if x.Guard.Committed() {
			x.committed = true // seen at the wire, e.g. a human committed by hand
		}
		if v := x.Guard.Take(); v != nil {
			obs, _ := x.Session.Observe(ctx)
			x.fail(ctx, s, obs, Failure{Kind: FailPolicy, Expected: "requests stay inside the allowlist", Observed: v.Detail, Message: v.Error()})
			return obs, sigStop
		}
		obs, err := x.Session.Observe(ctx)
		if err != nil {
			// A snapshot can fail while a frame is being replaced; only a
			// surface that stays unreadable is a failure.
			if observeErrors++; observeErrors < 3 && ctx.Err() == nil {
				time.Sleep(x.Opts.Poll)
				continue
			}
			x.fail(ctx, s, nil, Failure{Kind: FailSurface, Message: err.Error()})
			return nil, sigStop
		}
		observeErrors = 0
		if st, detail := x.Detect(obs); st != nil {
			sig, resume := x.onState(ctx, s, st, detail, obs)
			if !resume {
				return obs, sig
			}
			deadline, clean = time.Now().Add(timeout), false
			continue
		}
		if pred(obs) {
			if waited := time.Since(start); clean && waited > slowThreshold {
				x.recovered(s, Recovery{Condition: "slow_response", Action: "waited", WaitedMS: waited.Milliseconds()})
			}
			return obs, sigNext
		}
		if time.Now().After(deadline) {
			return obs, sigTimeout
		}
		select {
		case <-ctx.Done():
			x.fail(ctx, s, obs, Failure{Kind: FailSurface, Message: "run cancelled: " + ctx.Err().Error()})
			return obs, sigStop
		case <-time.After(x.Opts.Poll):
		}
	}
}

// onState responds to a known application state. resume reports whether the
// caller should keep waiting for what it was waiting for.
func (x *exec) onState(ctx context.Context, s *capability.Step, st *capability.State, detail string, obs *surface.Observation) (sig signal, resume bool) {
	x.Run.Log("automation", "state.detected", stepID(s), map[string]any{"state": st.ID, "class": st.Class, "detail": detail})

	switch st.Class {
	case capability.ClassBusiness:
		if !slices.ContainsFunc(x.art.Outcomes, func(o capability.Outcome) bool { return o.Code == st.ID }) {
			x.fail(ctx, s, obs, Failure{Kind: FailUnexpectedState, Expected: "one of the outcomes this capability declares",
				Observed: fmt.Sprintf("%s: %s", st.ID, detail), Message: "the application reported an outcome the capability's contract does not declare"})
			return sigStop, false
		}
		x.res.Status = StatusOutcome
		x.res.Outputs = nil
		x.res.Outcome = &Outcome{Code: st.ID, Message: detail, AtStep: stepID(s)}
		x.Snapshot(ctx, "outcome-"+st.ID, obs)
		return sigStop, false

	case capability.ClassFatal:
		x.fail(ctx, s, obs, Failure{Kind: FailFatalState, Observed: fmt.Sprintf("%s: %s", st.ID, detail), Message: st.Description})
		return sigStop, false

	case capability.ClassNeedsHuman:
		res, err := x.escalate(ctx, s, obs, control.KindNeedsHuman, st.Description, "")
		if err != nil {
			kind := FailUnanswered
			if errors.Is(err, runner.ErrNoOperator) {
				kind = FailNeedsHuman
			}
			x.fail(ctx, s, obs, Failure{Kind: kind, Observed: fmt.Sprintf("%s: %s", st.ID, detail), Message: st.Description + " (" + err.Error() + ")"})
			return sigStop, false
		}
		switch res.Action {
		case control.Resume:
			return sigNext, true
		case control.StepDone:
			if s == nil {
				// There is no step to have done by hand here; whatever is
				// being waited for is still verified.
				return sigNext, true
			}
			return sigStepDone, false
		}
		x.fail(ctx, s, obs, Failure{Kind: FailAborted, Observed: st.ID, Message: "operator " + res.Operator + " stopped the run: " + res.Note})
		return sigStop, false
	}

	// Recoverable.
	if st.Recover.Action == capability.RecoverRestart && x.committed {
		// Starting over would repeat a step that may already have committed.
		// That decision is never automation's to make. If an operator says
		// to carry on, the wait that was interrupted is still evaluated.
		if sig := x.indeterminate(ctx, s, obs, fmt.Sprintf("%s appeared after a committing step was dispatched", st.ID)); sig != sigNext {
			return sig, false
		}
		return sigNext, true
	}
	x.tries[st.ID]++
	attempt := x.tries[st.ID]
	if attempt > st.Recover.MaxAttempts {
		x.fail(ctx, s, obs, Failure{Kind: FailRecovery, Observed: fmt.Sprintf("%s: %s", st.ID, detail),
			Message: fmt.Sprintf("%s kept recurring after %d recovery attempt(s)", st.ID, st.Recover.MaxAttempts)})
		return sigStop, false
	}
	switch st.Recover.Action {
	case capability.RecoverDismiss:
		if err := x.Dismiss(ctx, st, obs); err != nil {
			x.fail(ctx, s, obs, Failure{Kind: FailTargetNotFound, Observed: st.ID, Message: err.Error()})
			return sigStop, false
		}
		x.recovered(s, Recovery{Condition: st.ID, Action: capability.RecoverDismiss, Attempt: attempt})
		return sigNext, true

	case capability.RecoverRestart:
		x.recovered(s, Recovery{Condition: st.ID, Action: capability.RecoverRestart, Attempt: attempt})
		return sigRestart, false
	}
	x.fail(ctx, s, obs, Failure{Kind: FailConfiguration, Message: "profile state " + st.ID + " has an unknown recovery action"})
	return sigStop, false
}

// step executes one recorded step: find the control, gate it, act, verify.
func (x *exec) step(ctx context.Context, s *capability.Step) signal {
	target, value, expect, err := x.render(s)
	if err != nil {
		x.fail(ctx, s, nil, Failure{Kind: FailConfiguration, Message: err.Error()})
		return sigStop
	}
	timeout := x.Opts.StepTimeout
	if s.TimeoutMS > 0 {
		timeout = time.Duration(s.TimeoutMS) * time.Millisecond
	}
	// A committing step must find its control by what it is, never by where.
	strict := s.Risk == policy.RiskIrreversible
	x.Run.Log("automation", "step.start", s.ID, map[string]any{"action": s.Action, "intent": s.Intent, "target": target.String(), "risk": s.Risk})

	var m locate.Match
	byHand, approved := false, false
	for {
		var miss error
		obs, sig := x.await(ctx, s, timeout, func(o *surface.Observation) bool {
			mm, err := locate.Resolve(target, o, strict)
			if err != nil {
				miss = err
				return false
			}
			m = mm
			return true
		})
		if sig == sigStepDone {
			byHand = true
			break
		}
		if sig == sigTimeout {
			// Stuck: the control is not there and nothing known explains why.
			res, err := x.escalate(ctx, s, obs, control.KindStuck, "the control for this step did not appear", target.String())
			if err != nil {
				kind := FailUnanswered
				if errors.Is(err, runner.ErrNoOperator) {
					kind = FailTargetNotFound
				}
				x.fail(ctx, s, obs, Failure{Kind: kind, Expected: target.String(), Observed: x.Observed(obs, target.Frame), Message: miss.Error()})
				return sigStop
			}
			switch res.Action {
			case control.Resume:
				continue
			case control.StepDone:
				byHand = true
			default:
				x.fail(ctx, s, obs, Failure{Kind: FailAborted, Expected: target.String(), Message: "operator " + res.Operator + " stopped the run: " + res.Note})
				return sigStop
			}
			break
		}
		if sig != sigNext {
			return sig
		}

		if err := x.Env.Policy.CheckAction(s.Action); err != nil {
			x.fail(ctx, s, obs, Failure{Kind: FailPolicy, Message: err.Error()})
			return sigStop
		}
		// The live control is re-classified: an artifact cannot downgrade
		// the risk of what it is about to click.
		risk := s.Risk
		if x.Env.Policy.ClassifyControl(s.Action, m.Element) == policy.RiskIrreversible {
			risk = policy.RiskIrreversible
		}
		if risk != policy.RiskIrreversible || approved || (x.Opts.AuthorizeIrreversible && x.approved) {
			if risk == policy.RiskIrreversible {
				x.Guard.Arm()
				x.committed = true
			} else if target.Frame != surface.DialogFrame {
				// An authorization covers the approved step and the dialog it
				// opens, nothing later.
				x.Guard.Disarm()
			}
			break
		}
		res, err := x.escalate(ctx, s, obs, control.KindApproval,
			fmt.Sprintf("this step commits an irreversible change (%s) and needs approval", m.Locator), target.String())
		if err != nil {
			kind := FailUnanswered
			if errors.Is(err, runner.ErrNoOperator) {
				kind = FailApprovalNeeded
			}
			x.fail(ctx, s, obs, Failure{Kind: kind, Expected: "approval for a committing step", Message: err.Error()})
			return sigStop
		}
		switch res.Action {
		case control.Approve, control.Resume:
			approved = true // loop once more: the screen may have moved while we waited
			continue
		case control.StepDone:
			byHand, x.committed = true, true
		case control.Deny:
			x.fail(ctx, s, obs, Failure{Kind: FailDenied, Message: "operator " + res.Operator + " declined the committing step: " + res.Note})
			return sigStop
		default:
			x.fail(ctx, s, obs, Failure{Kind: FailAborted, Message: "operator " + res.Operator + " stopped the run: " + res.Note})
			return sigStop
		}
		break
	}

	if byHand {
		if s.Action == capability.ActionExtract {
			x.fail(ctx, s, nil, Failure{Kind: FailOutputMissing, Expected: fmt.Sprintf("output %q read from %s", s.Output, target),
				Message: "a value cannot be read by hand; the output was not captured"})
			return sigStop
		}
		if s.Risk == policy.RiskIrreversible {
			x.committed = true // a person committing it counts exactly as much
		}
	} else {
		if m.Rank > 0 {
			d := Drift{Step: s.ID, Used: m.Locator.String(), Missed: m.Misses}
			x.res.Drift = append(x.res.Drift, d)
			x.Run.Log("automation", "locator.fallback", s.ID, d)
		}
		if sig := x.act(ctx, s, m, target, value); sig != sigNext {
			return sig
		}
	}

	if expect != nil {
		for {
			obs, sig := x.await(ctx, s, timeout, func(o *surface.Observation) bool { return locate.OnScreen(o, *expect) })
			if sig == sigNext || sig == sigStepDone {
				break
			}
			if sig != sigTimeout {
				return sig
			}
			if x.committed {
				if sig := x.indeterminate(ctx, s, obs, "the screen expected after a committing step did not appear"); sig != sigNext {
					return sig
				}
				break
			}
			res, err := x.escalate(ctx, s, obs, control.KindStuck, "the step was performed but the screen it should lead to did not appear", expect.String())
			if err != nil {
				kind := FailUnanswered
				if errors.Is(err, runner.ErrNoOperator) {
					kind = FailCheckpoint
				}
				x.fail(ctx, s, obs, Failure{Kind: kind, Expected: expect.String(), Observed: x.Observed(obs, expect.Frame),
					Message: "the step was performed but the screen it should lead to did not appear"})
				return sigStop
			}
			if res.Action == control.Resume {
				continue
			}
			if res.Action != control.StepDone {
				x.fail(ctx, s, obs, Failure{Kind: FailAborted, Expected: expect.String(), Message: "operator " + res.Operator + " stopped the run: " + res.Note})
				return sigStop
			}
			break
		}
	}
	x.Run.Log("automation", "step.ok", s.ID, map[string]any{"locator": m.Locator.String(), "fallback_rank": m.Rank, "done_by_human": byHand})
	return sigNext
}

// act performs the step's action and the verification that belongs to it.
func (x *exec) act(ctx context.Context, s *capability.Step, m locate.Match, target locate.Target, value string) signal {
	surfaceFail := func(err error) signal {
		x.fail(ctx, s, nil, Failure{Kind: FailSurface, Expected: target.String(), Message: err.Error()})
		return sigStop
	}
	switch s.Action {
	case capability.ActionClick:
		if err := x.Session.Click(ctx, m.Element); err != nil {
			return surfaceFail(err)
		}

	case capability.ActionType, capability.ActionSelect:
		var err error
		if s.Action == capability.ActionType {
			err = x.Session.Type(ctx, m.Element, value)
		} else {
			err = x.Session.Select(ctx, m.Element, value)
		}
		if err != nil {
			return surfaceFail(err)
		}
		// Read the field back: never assume input landed.
		obs, err := x.Session.Observe(ctx)
		if err != nil {
			return surfaceFail(err)
		}
		again, err := locate.Resolve(target, obs, false)
		if err != nil {
			return surfaceFail(fmt.Errorf("field disappeared after input: %w", err))
		}
		got := again.Element.Value
		ok := locate.Norm(got) == locate.Norm(value)
		if again.Element.Protected {
			ok = utf8.RuneCountInString(got) == utf8.RuneCountInString(value)
		}
		if !ok {
			f := Failure{Kind: FailReadback, Expected: "the field to hold the value entered", Observed: "the field holds a different value",
				Message: "input did not land in the field as entered"}
			if !again.Element.Protected && !capability.UsesSensitive(s.Value, x.art.Inputs) {
				f.Expected, f.Observed = fmt.Sprintf("field value %q", value), fmt.Sprintf("field value %q", got)
			}
			x.fail(ctx, s, obs, f)
			return sigStop
		}

	case capability.ActionExtract:
		raw := m.Element.Name
		if m.Element.Role != surface.RoleText {
			raw = m.Element.Value
		}
		var decl capability.Output
		for _, o := range x.art.Outputs {
			if o.Name == s.Output {
				decl = o
			}
		}
		v, err := capability.ParseValue(decl.Type, raw)
		if err != nil {
			x.fail(ctx, s, nil, Failure{Kind: FailOutputInvalid, Expected: fmt.Sprintf("a %s value for output %q", decl.Type, decl.Name),
				Observed: raw, Message: err.Error()})
			return sigStop
		}
		if decl.Sensitive {
			x.Redactor.AddLiteral("output", decl.Name, raw)
			x.Redactor.AddLiteral("output", decl.Name, fmt.Sprint(v))
		}
		x.res.Outputs[decl.Name] = v
	}
	return sigNext
}

// indeterminate handles the one situation automation must never resolve by
// itself: a committing step went out and its effect is unknown.
func (x *exec) indeterminate(ctx context.Context, s *capability.Step, obs *surface.Observation, why string) signal {
	for {
		res, err := x.escalate(ctx, s, obs, control.KindIndeterminate,
			why+"; do not repeat the step — check in the application whether it took effect", "")
		if err != nil {
			x.fail(ctx, s, obs, Failure{Kind: FailIndeterminate, Observed: x.Observed(obs, ""),
				Message: why + "; the run stopped without retrying (" + err.Error() + ")"})
			return sigStop
		}
		switch res.Action {
		case control.StepDone, control.Resume:
			// The operator has checked. Automation still verifies what
			// follows (the next steps and the final checkpoint) itself.
			return sigNext
		}
		x.fail(ctx, s, obs, Failure{Kind: FailAborted, Message: "operator " + res.Operator + " stopped the run: " + res.Note})
		return sigStop
	}
}

func (x *exec) escalate(ctx context.Context, s *capability.Step, obs *surface.Observation, kind control.Kind, reason, expected string) (control.Resolution, error) {
	req := control.Request{Kind: kind, Capability: x.art.ID + "@" + x.art.Version, Goal: x.art.Title, Reason: reason, Expected: expected}
	if s != nil {
		req.StepID, req.StepIntent = s.ID, s.Intent
	}
	res, err := x.Escalate(ctx, req, obs)
	if err != nil {
		return res, err
	}
	x.res.Handoffs = append(x.res.Handoffs, control.Record{Kind: kind, AtStep: stepID(s), Reason: reason,
		Resolution: res.Action, Operator: res.Operator, Note: res.Note, Actions: res.Actions})
	return res, nil
}

func (x *exec) recovered(s *capability.Step, r Recovery) {
	r.AtStep = stepID(s)
	x.res.Recoveries = append(x.res.Recoveries, r)
	x.Run.Log("automation", "recovery", r.AtStep, r)
}

// fail records a hard failure with evidence. It is the only place a run
// becomes "failed".
func (x *exec) fail(ctx context.Context, s *capability.Step, obs *surface.Observation, f Failure) {
	if s != nil {
		f.Step, f.Intent = s.ID, s.Intent
	}
	f.IrreversibleDispatched = x.committed || x.Guard.Committed()
	if x.opened {
		f.Screenshot, f.Observation = x.Snapshot(ctx, "failure", obs)
	}
	x.res.Status = StatusFailed
	x.res.Failure = &f
	x.res.Outputs = nil
	x.Run.Log("automation", "replay.failure", f.Step, f)
}

// render fills the step's templates with this invocation's inputs.
func (x *exec) render(s *capability.Step) (locate.Target, string, *locate.Screen, error) {
	target := s.Target
	target.Locators = make([]locate.Locator, len(s.Target.Locators))
	for i, l := range s.Target.Locators {
		for _, f := range []*string{&l.Name, &l.Label, &l.Row, &l.Column} {
			v, err := capability.Render(*f, x.inputs, nil)
			if err != nil {
				return target, "", nil, fmt.Errorf("step %s locator: %w", s.ID, err)
			}
			*f = v
		}
		target.Locators[i] = l
	}
	value, err := capability.Render(s.Value, x.inputs, x.secrets)
	if err != nil {
		return target, "", nil, fmt.Errorf("step %s value: %w", s.ID, err)
	}
	var expect *locate.Screen
	if s.Expect != nil {
		e := *s.Expect
		if e.Path, err = capability.Render(e.Path, x.inputs, nil); err != nil {
			return target, "", nil, fmt.Errorf("step %s expect: %w", s.ID, err)
		}
		if e.Title, err = capability.Render(e.Title, x.inputs, nil); err != nil {
			return target, "", nil, fmt.Errorf("step %s expect: %w", s.ID, err)
		}
		expect = &e
	}
	return target, value, expect, nil
}

// renderCondition fills input placeholders in a condition's texts.
func (x *exec) renderCondition(c locate.Condition) (locate.Condition, error) {
	var err error
	if c.Text, err = capability.Render(c.Text, x.inputs, nil); err != nil {
		return c, err
	}
	if c.Screen != nil {
		sc := *c.Screen
		if sc.Path, err = capability.Render(sc.Path, x.inputs, nil); err != nil {
			return c, err
		}
		if sc.Title, err = capability.Render(sc.Title, x.inputs, nil); err != nil {
			return c, err
		}
		c.Screen = &sc
	}
	all, any := c.All, c.Any
	c.All, c.Any = nil, nil
	for _, sub := range all {
		r, err := x.renderCondition(sub)
		if err != nil {
			return c, err
		}
		c.All = append(c.All, r)
	}
	for _, sub := range any {
		r, err := x.renderCondition(sub)
		if err != nil {
			return c, err
		}
		c.Any = append(c.Any, r)
	}
	return c, nil
}

func stepID(s *capability.Step) string {
	if s == nil {
		return ""
	}
	return s.ID
}
