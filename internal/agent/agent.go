// Package agent is the discovery path: a model works out how to accomplish a
// goal on a live surface, and the run is recorded as a capability artifact.
//
// The division of labour is deliberate. The model only ever proposes: which
// element, which action, which anchors identify a value, what proves success.
// The harness disposes: it checks every proposal against policy, derives the
// locators itself, verifies each one resolves back to the same element, and
// refuses to record anything it could not verify. The artifact is therefore
// built from checked facts about the screen, not from the model's prose.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/control"
	"github.com/NightWatchEng/rote/internal/llm"
	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/runner"
	"github.com/NightWatchEng/rote/internal/surface"
)

// Options are the agent's stopping conditions.
type Options struct {
	MaxTurns int
	Timeout  time.Duration
	// MaxStrikes is how many consecutive rejected or ineffective actions
	// count as "stuck".
	MaxStrikes int
}

func (o *Options) defaults() {
	if o.MaxTurns == 0 {
		o.MaxTurns = 30
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Minute
	}
	if o.MaxStrikes == 0 {
		o.MaxStrikes = 3
	}
}

// Result is what a discovery run reports.
type Result struct {
	Status        string           `json:"status"` // success | failed
	Reason        string           `json:"reason,omitempty"`
	Capability    string           `json:"capability"`
	Artifact      string           `json:"artifact,omitempty"`
	Turns         int              `json:"turns"`
	StepsRecorded int              `json:"steps_recorded"`
	Backend       string           `json:"backend"`
	Model         string           `json:"model"`
	InputTokens   int64            `json:"input_tokens"`
	OutputTokens  int64            `json:"output_tokens"`
	CostUSD       float64          `json:"cost_usd,omitempty"`
	Outputs       map[string]any   `json:"outputs,omitempty"`
	Handoffs      []control.Record `json:"handoffs,omitempty"`
	HumanAssisted bool             `json:"human_assisted"`
	DurationMS    int64            `json:"duration_ms"`
	RunID         string           `json:"run_id"`
	Evidence      string           `json:"evidence"`
}

// Agent runs discovery.
type Agent struct {
	*runner.Kit
	LLM  llm.Client
	Opts Options
}

type frameState struct{ path, title string }

type run struct {
	*Agent
	spec    *Spec
	inputs  map[string]string
	secrets map[string]string
	res     *Result

	steps    []capability.Step
	captured map[string]bool
	success  locate.Condition
	history  []string
	frames   map[string]frameState
	strikes  int
	// approved is the target of a committing click an operator has already
	// said yes to, so a re-planned turn does not ask twice; approvedWhy is
	// the reason the model gave when it first proposed that click.
	approved, approvedWhy string
}

// Discover pursues the spec's goal and, on success, writes the artifact to
// outPath.
func (a *Agent) Discover(ctx context.Context, spec *Spec, inputs map[string]string, outPath string) *Result {
	a.Opts.defaults()
	ctx, cancel := context.WithTimeout(ctx, a.Opts.Timeout)
	defer cancel()
	start := time.Now()

	res := &Result{Status: "failed", Capability: spec.ID, Backend: a.LLM.Backend(), Model: a.LLM.Model(), RunID: a.Run.ID, Evidence: a.Run.Dir}
	r := &run{Agent: a, spec: spec, inputs: inputs, res: res, captured: map[string]bool{}, frames: map[string]frameState{}}
	res.Outputs = map[string]any{}

	for _, p := range spec.Inputs {
		if p.Sensitive {
			a.Redactor.AddLiteral("input", p.Name, inputs[p.Name])
		}
	}
	a.Run.Log("automation", "discovery.start", "", map[string]any{
		"capability": spec.ID, "goal": spec.Goal, "tenant": a.Env.Tenant.ID, "inputs": capability.MaskSensitive(spec.Inputs, inputs),
		"backend": res.Backend, "model": res.Model, "operator_attached": a.Escalator != nil,
	})

	if r.prepare(ctx) {
		r.loop(ctx)
	}
	if res.Status == "success" {
		art, err := r.artifact()
		if err == nil {
			err = art.Save(outPath)
		}
		if err != nil {
			res.Status, res.Reason = "failed", "the run succeeded but the artifact was not saved: "+err.Error()
		} else {
			res.Artifact = outPath
			a.Run.SaveJSON("artifact.json", art)
		}
	}
	if res.Status != "success" {
		a.Snapshot(ctx, "failure", nil)
	}

	res.StepsRecorded = len(r.steps)
	res.DurationMS = time.Since(start).Milliseconds()
	a.Run.Log("automation", "discovery.result", "", res)
	a.Run.SaveJSON("discovery.json", res)
	return res
}

func (r *run) stop(reason string) {
	r.res.Status, r.res.Reason = "failed", reason
}

// prepare validates inputs, opens the application and confirms it is the
// product the tenant's profile describes.
func (r *run) prepare(ctx context.Context) bool {
	if err := capability.CheckParams(r.spec.Inputs, r.inputs); err != nil {
		r.stop("invalid input: " + err.Error())
		return false
	}
	var err error
	if r.secrets, err = r.Env.Secrets(r.spec.Secrets); err != nil {
		r.stop(err.Error())
		return false
	}
	for name, v := range r.secrets {
		r.Redactor.AddLiteral("secret", name, v)
	}
	if err := r.Session.Open(ctx, r.Env.EntryURL(r.spec.Entry)); err != nil {
		r.stop("could not open the entry point: " + err.Error())
		return false
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		obs, err := surface.Settle(ctx, r.Session, 3*time.Second)
		if err != nil {
			r.stop("could not observe the application: " + err.Error())
			return false
		}
		if ok, _ := r.Env.Profile.Fingerprint.Eval(obs); ok {
			r.screenChange(obs)
			return true
		}
		if time.Now().After(deadline) {
			r.stop("the entry point does not show the expected application (" + r.Env.Profile.Fingerprint.String() + ")")
			return false
		}
	}
}

// loop is observe -> decide -> act until the goal is met or a stopping
// condition is hit.
func (r *run) loop(ctx context.Context) {
	dismissals := 0
	for turn := 1; turn <= r.Opts.MaxTurns; turn++ {
		r.res.Turns = turn
		if ctx.Err() != nil {
			r.stop("time budget exhausted")
			return
		}
		obs, err := surface.Settle(ctx, r.Session, 6*time.Second)
		if err != nil {
			r.stop("could not observe the application: " + err.Error())
			return
		}
		if v := r.Guard.Take(); v != nil {
			r.reject("the previous action", "it led somewhere this automation is not permitted to go ("+v.Detail+")")
		}
		// Known interruptions are cleared by the harness, exactly as replay
		// would, so they never become recorded steps of the flow.
		if st, _ := r.Detect(obs); st != nil && st.Class == capability.ClassRecoverable &&
			st.Recover.Action == capability.RecoverDismiss && dismissals < 5 {
			dismissals++
			if err := r.Dismiss(ctx, st, obs); err == nil {
				r.Run.Log("automation", "recovery", "", map[string]any{"condition": st.ID, "action": "dismiss"})
				turn--
				continue
			}
		}

		if r.strikes >= r.Opts.MaxStrikes {
			if !r.handover(ctx, obs, fmt.Sprintf("the agent made no progress in %d consecutive actions", r.strikes)) {
				return
			}
			continue
		}

		safe, masked := r.Redactor.Observation(obs)
		if png, err := r.Session.Screenshot(ctx); err == nil {
			r.Run.Screenshot(fmt.Sprintf("turn-%02d.png", turn), png, masked)
		}
		dec, err := r.decide(ctx, buildPrompt(r.spec, r.captured, r.history, safe))
		if err != nil {
			r.stop("model error: " + err.Error())
			return
		}
		entry := map[string]any{"turn": turn, "action": dec.Action, "reason": dec.Reason}
		if el, ok := safe.ByRef(dec.Ref); ok && dec.Ref != 0 {
			entry["element"] = fmt.Sprintf("%s %q in frame %s", el.Role, el.Name, el.Frame)
		}
		if dec.Text != "" {
			entry["text"] = dec.Text
		}
		if dec.Output != "" {
			entry["output"], entry["row_anchor"], entry["column_anchor"] = dec.Output, dec.RowAnchor, dec.ColumnAnchor
		}
		r.Run.Log("automation", "agent.decision", "", entry)

		switch dec.Action {
		case capability.ActionClick, capability.ActionType, capability.ActionSelect:
			if !r.perform(ctx, obs, dec) {
				return
			}
		case capability.ActionExtract:
			r.extract(obs, dec)
		case "finish":
			if r.finish(obs, dec) {
				return
			}
		case "escalate":
			if !r.handover(ctx, obs, "the agent asked for a human: "+dec.Reason) {
				return
			}
		default:
			r.reject("action "+dec.Action, "it is not one of the available actions")
		}
	}
	if r.res.Status != "success" && r.res.Reason == "" {
		r.stop(fmt.Sprintf("step budget of %d turns exhausted", r.Opts.MaxTurns))
	}
}

func (r *run) decide(ctx context.Context, prompt string) (Decision, error) {
	var dec Decision
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := r.LLM.Complete(ctx, llm.Request{System: systemPrompt, Prompt: prompt, Schema: decisionSchema})
		if err != nil {
			return dec, err
		}
		r.res.InputTokens += resp.InputTokens
		r.res.OutputTokens += resp.OutputTokens
		r.res.CostUSD += resp.CostUSD
		if last = json.Unmarshal(resp.JSON, &dec); last == nil {
			return dec, nil
		}
	}
	return dec, fmt.Errorf("the model did not return a valid decision: %w", last)
}

// note adds a line to what the model is told has happened.
func (r *run) note(line string) {
	r.history = append(r.history, r.Redactor.Text(line))
}

// reject refuses a proposal and tells the model why. Rejections are strikes.
func (r *run) reject(what, why string) {
	r.strikes++
	r.note(fmt.Sprintf("REJECTED %s: %s", what, why))
	r.Run.Log("policy", "agent.rejected", "", map[string]any{"proposal": what, "why": why, "strikes": r.strikes})
}

// perform checks, executes and records a click, type or select. It returns
// false if the run must stop.
func (r *run) perform(ctx context.Context, obs *surface.Observation, dec Decision) bool {
	el, ok := obs.ByRef(dec.Ref)
	if !ok {
		r.reject(fmt.Sprintf("%s on ref %d", dec.Action, dec.Ref), "there is no such element on the current screen")
		return true
	}
	what := fmt.Sprintf("%s on %s %q", dec.Action, el.Role, el.Name)

	// 1. Policy: is this kind of action, on this control, allowed at all?
	if err := r.Env.Policy.CheckAction(dec.Action); err != nil {
		r.reject(what, err.Error())
		return true
	}
	if el.Href != "" {
		if err := r.Env.Policy.CheckURL(el.Href); err != nil {
			r.reject(what, err.Error())
			return true
		}
	}
	if dec.Action == capability.ActionType && el.Role != surface.RoleTextbox {
		r.reject(what, "only a textbox can be typed into")
		return true
	}
	if dec.Action == capability.ActionSelect && el.Role != surface.RoleSelect {
		r.reject(what, "only a select can have an option chosen")
		return true
	}

	// 2. Values: turned into a template before anything else sees them.
	var template, value string
	if dec.Action != capability.ActionClick {
		template = r.parameterize(dec.Text)
		if err := r.checkTemplate(template); err != nil {
			r.reject(what, err.Error())
			return true
		}
		var err error
		if value, err = capability.Render(template, r.inputs, r.secrets); err != nil {
			r.reject(what, err.Error())
			return true
		}
	}

	// 3. Locators: derived by the harness and verified against the screen.
	target, err := locate.Derive(obs, el)
	if err != nil {
		r.reject(what, err.Error())
		return true
	}

	// 4. Risk: a committing action needs a human's yes, every time. The
	// approval is for this control on this exact screen: any other action,
	// or the same control after the screen changed, voids it.
	risk := r.Env.Policy.ClassifyControl(dec.Action, el)
	approval := target.String() + " on screen " + obs.Fingerprint()
	if r.approved != approval {
		r.approved, r.approvedWhy = "", ""
	}
	if el.Frame != surface.DialogFrame && risk != policy.RiskIrreversible {
		// An earlier authorization covered that step and the dialog it
		// opened, nothing later.
		r.Guard.Disarm()
	}
	if risk == policy.RiskIrreversible && !hasSemanticLocator(target) {
		r.reject(what, "it commits an irreversible change but can only be identified by position, which is not reliable enough to replay")
		return true
	}
	if risk == policy.RiskIrreversible && r.approved != approval {
		res, err := r.Escalate(ctx, control.Request{
			Kind: control.KindApproval, Capability: r.spec.ID, Goal: r.spec.Goal, StepIntent: dec.Reason,
			Reason:   fmt.Sprintf("the agent wants to click %q, which commits an irreversible change", el.Name),
			Expected: target.String(),
		}, obs)
		if errors.Is(err, runner.ErrNoOperator) {
			r.reject(what, "it commits an irreversible change, which needs an operator's approval, and no operator is attached to this run")
			return true
		}
		if err != nil {
			r.stop("approval was requested and not answered: " + err.Error())
			return false
		}
		r.record(control.KindApproval, res, "irreversible action: "+what)
		switch res.Action {
		case control.Approve, control.Resume:
			// Re-plan on a fresh screen; the approval is remembered.
			r.approved, r.approvedWhy = approval, dec.Reason
			r.note(fmt.Sprintf("An operator approved %s. Proceed with it.", what))
			return true
		case control.StepDone:
			r.note(fmt.Sprintf("An operator performed %s by hand. Re-read the screen.", what))
			r.resync(ctx)
			return true
		case control.Deny:
			r.reject(what, "an operator declined it")
			return true
		default:
			r.stop("an operator stopped the run: " + res.Note)
			return false
		}
	}
	if risk == policy.RiskIrreversible {
		r.Guard.Arm()
		dec.Reason = r.approvedWhy // the step's purpose, not "because it was approved"
		r.approved, r.approvedWhy = "", ""
	}

	// 5. Act, then look at what happened.
	before := obs.Fingerprint()
	switch dec.Action {
	case capability.ActionClick:
		err = r.Session.Click(ctx, el)
	case capability.ActionType:
		err = r.Session.Type(ctx, el, value)
	case capability.ActionSelect:
		err = r.Session.Select(ctx, el, value)
	}
	if err != nil {
		r.reject(what, "the action failed: "+err.Error())
		return true
	}
	limit := 5 * time.Second
	if dec.Action != capability.ActionClick {
		limit = 2 * time.Second
	}
	after, changed := r.AwaitChange(ctx, before, limit)
	if v := r.Guard.Take(); v != nil {
		// The action was stopped at the network boundary. It is refused, not
		// recorded: an artifact must never contain a step policy forbids.
		r.reject(what, "it led somewhere this automation is not permitted to go ("+v.Detail+")")
		r.screenChange(after)
		return true
	}
	if !changed {
		// An action with no observable effect is not part of the flow.
		r.strikes++
		r.note(fmt.Sprintf("%s — nothing on screen changed; not recorded", what))
		return true
	}
	r.strikes = 0

	// 6. Record.
	step := capability.Step{
		ID:     fmt.Sprintf("s%d", len(r.steps)+1),
		Intent: r.Redactor.Text(dec.Reason),
		Action: dec.Action,
		Target: r.portable(target),
		Value:  template,
		Risk:   risk,
		Expect: r.screenChange(after),
	}
	r.steps = append(r.steps, step)
	r.Run.Log("automation", "agent.step_recorded", step.ID, step)

	line := what
	if template != "" {
		line += " with " + template
	}
	if step.Expect != nil {
		line += " — now showing " + step.Expect.String()
	} else {
		line += " — done"
	}
	r.note(line)
	return true
}

// extract records where a declared output is read from.
func (r *run) extract(obs *surface.Observation, dec Decision) {
	what := "extract " + dec.Output
	i := slices.IndexFunc(r.spec.Outputs, func(o capability.Output) bool { return o.Name == dec.Output })
	if i < 0 {
		r.reject(what, "no such output is declared")
		return
	}
	decl := r.spec.Outputs[i]
	el, ok := obs.ByRef(dec.Ref)
	if !ok {
		r.reject(what, fmt.Sprintf("there is no element with ref %d on the current screen", dec.Ref))
		return
	}
	target, err := locate.DeriveValue(obs, el, dec.RowAnchor, dec.ColumnAnchor)
	if err != nil {
		r.reject(what, err.Error())
		return
	}
	raw := el.Name
	if el.Role != surface.RoleText {
		raw = el.Value
	}
	v, err := capability.ParseValue(decl.Type, raw)
	if err != nil {
		r.reject(what, fmt.Sprintf("that element does not hold a %s value", decl.Type))
		return
	}
	if decl.Sensitive {
		r.Redactor.AddLiteral("output", decl.Name, raw)
		r.Redactor.AddLiteral("output", decl.Name, fmt.Sprint(v))
	}
	// A later extraction of the same output supersedes an earlier one.
	r.steps = slices.DeleteFunc(r.steps, func(s capability.Step) bool {
		return s.Action == capability.ActionExtract && s.Output == decl.Name
	})
	for n := range r.steps {
		r.steps[n].ID = fmt.Sprintf("s%d", n+1)
	}
	step := capability.Step{
		ID: fmt.Sprintf("s%d", len(r.steps)+1), Intent: r.Redactor.Text(dec.Reason), Action: capability.ActionExtract,
		Target: r.portable(target), Output: decl.Name, Risk: policy.RiskRead,
	}
	r.steps = append(r.steps, step)
	r.captured[decl.Name] = true
	r.res.Outputs[decl.Name] = v
	r.strikes = 0
	r.Run.Log("automation", "agent.step_recorded", step.ID, step)
	r.note(fmt.Sprintf("extracted %s from %s", decl.Name, target.Locators[0]))
}

// finish checks the model's claim that the goal is met. It returns true if
// the run is over.
func (r *run) finish(obs *surface.Observation, dec Decision) bool {
	if !dec.Success {
		r.stop("the agent reported the goal cannot be achieved: " + r.Redactor.Text(dec.Reason))
		return true
	}
	var missing []string
	for _, o := range r.spec.Outputs {
		if !r.captured[o.Name] {
			missing = append(missing, o.Name)
		}
	}
	if len(missing) > 0 {
		r.reject("finish", "these outputs have not been extracted yet: "+strings.Join(missing, ", "))
		return false
	}
	if st, detail := r.Detect(obs); st != nil && st.Class != capability.ClassRecoverable {
		r.reject("finish", fmt.Sprintf("the screen shows %q, which is not the goal state", detail))
		return false
	}
	text := strings.TrimSpace(dec.Text)
	if text == "" {
		r.reject("finish", "text must name something visible that proves the goal state")
		return false
	}
	if r.Redactor.Text(text) != text || r.parameterize(text) != text {
		r.reject("finish", "the proof must be static text such as a heading, not a data value")
		return false
	}
	cond := locate.Condition{Text: text}
	if ok, _ := cond.Eval(obs); !ok {
		r.reject("finish", fmt.Sprintf("the text %q is not visible on the current screen", text))
		return false
	}
	for _, e := range obs.Elements {
		if strings.Contains(locate.Norm(e.Name), locate.Norm(text)) {
			cond.Frame = e.Frame
			break
		}
	}
	cond.Text = r.Env.CanonicalText(text)
	// The proof text says the goal screen was reached; the inputs visible
	// on it say it was reached for the right record. Only string inputs
	// are used: a money or integer value is displayed in too many formats.
	r.success = locate.Condition{All: []locate.Condition{cond}}
	for _, p := range r.spec.Inputs {
		v := r.inputs[p.Name]
		if p.Sensitive || (p.Type != "string" && p.Type != "enum") || len(v) < 3 {
			continue
		}
		if ok, _ := (locate.Condition{Text: v, Frame: cond.Frame}).Eval(obs); ok {
			r.success.All = append(r.success.All, locate.Condition{Text: "{{inputs." + p.Name + "}}", Frame: cond.Frame})
		}
	}
	if len(r.success.All) == 1 {
		r.success = cond
	}
	r.res.Status, r.res.Reason = "success", ""
	return true
}

// handover gives the live session to a human because the agent is stuck. It
// returns false if the run must stop.
func (r *run) handover(ctx context.Context, obs *surface.Observation, reason string) bool {
	res, err := r.Escalate(ctx, control.Request{Kind: control.KindStuck, Capability: r.spec.ID, Goal: r.spec.Goal, Reason: reason}, obs)
	if errors.Is(err, runner.ErrNoOperator) {
		r.stop(reason + "; no operator is attached to take over")
		return false
	}
	if err != nil {
		r.stop(reason + "; " + err.Error())
		return false
	}
	r.record(control.KindStuck, res, reason)
	if res.Action == control.Abort || res.Action == control.Deny {
		r.stop("an operator stopped the run: " + res.Note)
		return false
	}
	r.strikes = 0
	r.note(fmt.Sprintf("A human operator took control, performed %d action(s) and handed the session back. Re-read the screen and continue.", len(res.Actions)))
	r.resync(ctx)
	return true
}

func (r *run) record(kind control.Kind, res control.Resolution, reason string) {
	if len(res.Actions) > 0 {
		// What a person did by hand is not in the recorded steps, so the
		// artifact says so rather than pretending to be complete.
		r.res.HumanAssisted = true
	}
	r.res.Handoffs = append(r.res.Handoffs, control.Record{Kind: kind, Reason: r.Redactor.Text(reason),
		Resolution: res.Action, Operator: res.Operator, Note: res.Note, Actions: res.Actions})
}

func hasSemanticLocator(t locate.Target) bool {
	return slices.ContainsFunc(t.Locators, func(l locate.Locator) bool { return l.Strategy != locate.ByOrdinal })
}

// resync absorbs whatever a human changed, so the next recorded step is not
// credited with their navigation.
func (r *run) resync(ctx context.Context) {
	if obs, err := surface.Settle(ctx, r.Session, 3*time.Second); err == nil {
		r.screenChange(obs)
	}
}

// screenChange compares the frames now on screen with what each last showed
// and returns the screen that changed, if any. It is how a step's Expect is
// recorded without the model being asked.
func (r *run) screenChange(after *surface.Observation) *locate.Screen {
	var changed *locate.Screen
	present := map[string]bool{}
	for _, f := range after.Frames {
		present[f.Name] = true
		cur := frameState{locate.PathOf(f.URL), f.Title}
		if prev, ok := r.frames[f.Name]; !ok || prev != cur {
			changed = &locate.Screen{Frame: f.Name, Path: r.parameterize(cur.path), Title: r.parameterize(r.Env.CanonicalText(cur.title))}
		}
		r.frames[f.Name] = cur
	}
	if !present[surface.DialogFrame] {
		delete(r.frames, surface.DialogFrame) // so the next dialog counts as new
	}
	return changed
}

// parameterize replaces any concrete input value in a string with its
// placeholder, longest value first.
func (r *run) parameterize(s string) string {
	names := make([]string, 0, len(r.inputs))
	for name := range r.inputs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return len(r.inputs[names[i]]) > len(r.inputs[names[j]]) })
	for _, name := range names {
		if v := r.inputs[name]; len(v) >= 3 {
			s = strings.ReplaceAll(s, v, "{{inputs."+name+"}}")
		}
	}
	return s
}

// checkTemplate makes sure a typed value only refers to declared inputs and
// secrets, and carries no literal that looks like sensitive data.
func (r *run) checkTemplate(tpl string) error {
	literal, err := capability.LiteralPart(tpl, r.spec.Inputs, r.spec.Secrets)
	if err != nil {
		return err
	}
	if r.Redactor.Text(literal) != literal {
		return errors.New("the text contains a literal that looks like sensitive data; supply it as an input instead")
	}
	return nil
}

// portable makes a target independent of this tenant and this invocation:
// tenant wording is mapped back to canonical labels, and input values in
// locator text become placeholders.
func (r *run) portable(t locate.Target) locate.Target {
	t = r.Env.CanonicalTarget(t)
	for i := range t.Locators {
		l := &t.Locators[i]
		l.Name, l.Label, l.Row, l.Column = r.parameterize(l.Name), r.parameterize(l.Label), r.parameterize(l.Row), r.parameterize(l.Column)
	}
	return t
}

// artifact assembles the capability from what was recorded, then proves it
// is self-consistent and free of concrete values before it is written.
func (r *run) artifact() (*capability.Artifact, error) {
	art := &capability.Artifact{
		SchemaVersion: capability.SchemaVersion,
		ID:            r.spec.ID,
		Version:       r.spec.Version,
		Title:         r.spec.Title,
		Description:   r.spec.Description,
		App: capability.AppRef{
			Profile: r.Env.Profile.ID, ProfileVersion: r.Env.Profile.Version, Surface: "web/accessibility-tree",
		},
		Inputs:   r.spec.Inputs,
		Outputs:  r.spec.Outputs,
		Entry:    r.spec.Entry,
		Steps:    r.steps,
		Success:  r.success,
		Approval: capability.Approval{Status: capability.StatusDraft},
		Provenance: capability.Provenance{
			RecordedAt: time.Now().UTC().Format(time.RFC3339), Goal: r.spec.Goal,
			Model: r.LLM.Model(), Backend: r.LLM.Backend(), RunID: r.Run.ID,
			Tenant: r.Env.Tenant.ID, HumanAssisted: r.res.HumanAssisted,
		},
	}
	if art.Version == "" {
		art.Version = "1.0.0"
	}
	if art.Inputs == nil {
		art.Inputs = []capability.Param{}
	}
	if art.Outputs == nil {
		art.Outputs = []capability.Output{}
	}
	art.Secrets = []string{}
	for _, name := range r.spec.Secrets {
		if slices.ContainsFunc(r.steps, func(s capability.Step) bool { return strings.Contains(s.Value, "secrets."+name) }) {
			art.Secrets = append(art.Secrets, name)
		}
	}
	// The contract's non-success outcomes are the product's business states.
	art.Outcomes = []capability.Outcome{}
	for _, st := range r.Env.Profile.States {
		if st.Class == capability.ClassBusiness {
			art.Outcomes = append(art.Outcomes, capability.Outcome{Code: st.ID, Description: st.Description})
		}
	}
	art.Risk = art.MaxRisk()
	if err := art.Validate(); err != nil {
		return nil, err
	}

	// Last check before anything is written: the recorded flow must hold
	// placeholders, never the values this particular run used.
	raw, _ := json.Marshal(struct {
		Steps   []capability.Step
		Success locate.Condition
	}{art.Steps, art.Success})
	for name, v := range r.secrets {
		if len(v) >= 3 && strings.Contains(string(raw), v) {
			return nil, fmt.Errorf("refusing to save: the artifact contains the value of secret %q", name)
		}
	}
	for name, v := range r.inputs {
		if len(v) >= 3 && strings.Contains(string(raw), v) {
			return nil, fmt.Errorf("refusing to save: the artifact contains the concrete value of input %q", name)
		}
	}
	return art, nil
}
