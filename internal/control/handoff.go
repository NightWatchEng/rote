package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Kind is why a human is being asked in.
type Kind string

const (
	// KindApproval: automation is about to take an irreversible step and
	// needs a person to say yes. No control transfer is required.
	KindApproval Kind = "approval"
	// KindStuck: automation cannot find a way forward.
	KindStuck Kind = "stuck"
	// KindNeedsHuman: the application is in a state only a person may
	// resolve (a supervisor override, a challenge question).
	KindNeedsHuman Kind = "needs_human"
	// KindIndeterminate: an irreversible step was dispatched and its result
	// could not be verified. Automation must not retry; a person must look.
	KindIndeterminate Kind = "indeterminate"
)

// Resolutions an operator can give.
const (
	// Approve / Deny answer an approval request without taking control.
	Approve = "approve"
	Deny    = "deny"
	// Resume: "I dealt with what stopped you — carry on from where you
	// paused." Automation re-observes and re-evaluates the same wait, so the
	// human's work is verified by automation's own checkpoint, not taken on
	// trust.
	Resume = "resume"
	// StepDone: "I performed your current step by hand — do not repeat it;
	// continue with the next one."
	StepDone = "step_done"
	// Abort: "Stop the run."
	Abort = "abort"
)

// Request is an intervention request: everything an operator needs to act
// without asking anyone what is going on.
type Request struct {
	Kind       Kind   `json:"kind"`
	RunID      string `json:"run_id"`
	Capability string `json:"capability"`
	Goal       string `json:"goal,omitempty"`
	StepID     string `json:"step_id,omitempty"`
	StepIntent string `json:"step_intent,omitempty"`
	Reason     string `json:"reason"`
	Expected   string `json:"expected,omitempty"`
	Location   string `json:"location,omitempty"`
	// Screen is the redacted visible text at the moment automation stopped;
	// Screenshot is the redacted image saved in the run's evidence. The
	// operator additionally gets the live, unredacted session.
	Screen     []string `json:"screen,omitempty"`
	Screenshot string   `json:"screenshot,omitempty"`
}

// Resolution is how an intervention ended.
type Resolution struct {
	Action   string        `json:"action"`
	Operator string        `json:"operator"`
	Note     string        `json:"note,omitempty"`
	Actions  []HumanAction `json:"human_actions,omitempty"`
}

// Record is the summary of one intervention that a run's result carries.
type Record struct {
	Kind       Kind          `json:"kind"`
	AtStep     string        `json:"at_step,omitempty"`
	Reason     string        `json:"reason"`
	Resolution string        `json:"resolution"`
	Operator   string        `json:"operator"`
	Note       string        `json:"note,omitempty"`
	Actions    []HumanAction `json:"human_actions,omitempty"`
}

// Intervention is a request plus its lifecycle.
type Intervention struct {
	ID         string      `json:"id"`
	Request    Request     `json:"request"`
	Status     string      `json:"status"` // pending | claimed | resolved | expired
	Operator   string      `json:"operator,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
	Resolution *Resolution `json:"resolution,omitempty"`
}

// Escalator is what the agent loop and replay engine call when they cannot
// safely continue. A run with no Escalator attached simply fails instead.
type Escalator interface {
	Escalate(ctx context.Context, req Request) (Resolution, error)
}

// ErrTimeout means nobody answered an intervention request in time.
var ErrTimeout = errors.New("no operator resolved the intervention in time")

// Hub routes intervention requests for one session to whoever is operating
// the console, and carries out the control transfer.
type Hub struct {
	sess    *Session
	timeout time.Duration
	// Notify is called when a request is raised (e.g. to print the console
	// address). It must not block.
	Notify func(*Intervention)

	mu       sync.Mutex
	seq      int
	current  *Intervention
	resolved chan Resolution
}

func NewHub(sess *Session, timeout time.Duration) *Hub {
	return &Hub{sess: sess, timeout: timeout}
}

// Escalate stops automation, publishes the request and blocks until an
// operator resolves it. On return, control is back with automation.
func (h *Hub) Escalate(ctx context.Context, req Request) (Resolution, error) {
	// Automation gives up the session before the request is published, so
	// an operator can never claim it while automation still holds it.
	h.sess.transfer(HolderPending, "", string(req.Kind)+": "+req.Reason)

	h.mu.Lock()
	h.seq++
	iv := &Intervention{ID: fmt.Sprintf("iv-%d", h.seq), Request: req, Status: "pending", CreatedAt: time.Now()}
	ch := make(chan Resolution, 1)
	h.current, h.resolved = iv, ch
	h.mu.Unlock()

	h.sess.run.Log("automation", "handoff.requested", req.StepID, map[string]any{"intervention": iv.ID, "request": req})
	if h.Notify != nil {
		cp := *iv
		h.Notify(&cp)
	}

	timeout := time.After(h.timeout)
	for {
		select {
		case res := <-ch:
			return res, nil
		case <-timeout:
			// The timeout is for a request nobody picked up. Once an operator
			// has claimed the session they are mid-task, and automation waits
			// for them rather than pulling the session away.
			h.mu.Lock()
			if iv.Status == "claimed" {
				h.mu.Unlock()
				timeout = nil
				continue
			}
			res, err := h.abandon(iv, ch, ErrTimeout)
			h.mu.Unlock()
			if err == nil {
				return res, nil
			}
			return h.expired(iv, err)
		case <-ctx.Done():
			h.mu.Lock()
			res, err := h.abandon(iv, ch, ctx.Err())
			h.mu.Unlock()
			if err == nil {
				return res, nil
			}
			return h.expired(iv, err)
		}
	}
}

// abandon closes an unanswered intervention. It must be called with h.mu
// held, which makes it atomic with Resolve: if an operator's resolution got
// in first it is returned and honoured; otherwise the intervention is closed
// and any later Claim or Resolve is refused.
func (h *Hub) abandon(iv *Intervention, ch chan Resolution, cause error) (Resolution, error) {
	select {
	case res := <-ch:
		return res, nil
	default:
	}
	iv.Status = "expired"
	h.current, h.resolved = nil, nil
	return Resolution{}, cause
}

func (h *Hub) expired(iv *Intervention, cause error) (Resolution, error) {
	h.sess.takeActions()
	h.sess.transfer(HolderAutomation, "", "intervention closed unanswered: "+cause.Error())
	h.sess.run.Log("system", "handoff.expired", iv.Request.StepID, map[string]any{"intervention": iv.ID, "cause": cause.Error()})
	return Resolution{}, cause
}

// Current returns the open intervention, if any.
func (h *Hub) Current() *Intervention {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil {
		return nil
	}
	cp := *h.current
	return &cp
}

func (h *Hub) open(id string) (*Intervention, error) {
	if h.current == nil || h.current.ID != id {
		return nil, fmt.Errorf("intervention %s is not open", id)
	}
	return h.current, nil
}

// Claim gives control of the live session to an operator.
func (h *Hub) Claim(id, operator string) error {
	if operator == "" {
		return errors.New("operator name is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	iv, err := h.open(id)
	if err != nil {
		return err
	}
	if iv.Status != "pending" {
		return fmt.Errorf("intervention %s is already claimed by %s", id, iv.Operator)
	}
	iv.Status, iv.Operator = "claimed", operator
	h.sess.transfer(HolderHuman, operator, "operator claimed "+id)
	return nil
}

// Input forwards one operator action to the live session.
func (h *Hub) Input(ctx context.Context, id, operator string, in Input) (HumanAction, error) {
	h.mu.Lock()
	_, err := h.open(id)
	h.mu.Unlock()
	if err != nil {
		return HumanAction{}, err
	}
	return h.sess.HumanInput(ctx, operator, in)
}

// Resolve ends the intervention and hands control back to automation.
func (h *Hub) Resolve(id, operator, action, note string) error {
	if operator == "" {
		return errors.New("operator name is required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	iv, err := h.open(id)
	if err != nil {
		return err
	}
	switch action {
	case Approve, Deny:
		if iv.Request.Kind != KindApproval {
			return fmt.Errorf("%s only answers an approval request; this is a %s request", action, iv.Request.Kind)
		}
	case Resume, StepDone:
		// These assert something about the state of the live session, so
		// only the operator actually holding it may say so.
		if iv.Status != "claimed" || iv.Operator != operator {
			return fmt.Errorf("%s requires that %s first take control of the session", action, operator)
		}
	case Abort:
	default:
		return fmt.Errorf("unknown resolution %q (valid: %v)", action,
			[]string{Approve, Deny, Resume, StepDone, Abort})
	}
	if iv.Status == "claimed" && iv.Operator != operator && action != Abort {
		return fmt.Errorf("intervention %s is held by %s", id, iv.Operator)
	}

	res := Resolution{Action: action, Operator: operator, Note: note, Actions: h.sess.takeActions()}
	iv.Status, iv.Resolution = "resolved", &res
	h.sess.run.Log("human:"+operator, "handoff.resolved", iv.Request.StepID, map[string]any{
		"intervention": id, "action": action, "note": note, "human_actions": len(res.Actions),
	})
	h.sess.transfer(HolderAutomation, "", "operator "+operator+" resolved "+id+" with "+action)
	h.resolved <- res
	h.current, h.resolved = nil, nil
	return nil
}

// Session exposes the live session to the operator surface (for the live
// view and the control state).
func (h *Hub) Session() *Session { return h.sess }
