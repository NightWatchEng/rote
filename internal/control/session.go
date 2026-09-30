// Package control owns the question "who is driving the live session right
// now?". A Session wraps a surface with a single control token that is held
// by the automation, by a named human operator, or by nobody while a handoff
// is pending. Every action goes through the token check, so automation cannot
// act while a human is in control and vice versa, and every transfer is
// recorded in the run's evidence.
package control

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/NightWatchEng/rote/internal/evidence"
	"github.com/NightWatchEng/rote/internal/surface"
)

// Holder is who may act on the session.
type Holder string

const (
	// HolderAutomation: the agent loop or the replay engine is driving.
	HolderAutomation Holder = "automation"
	// HolderPending: automation has stopped and asked for a human; nobody
	// may act until an operator claims the session or resolves the request.
	HolderPending Holder = "pending_human"
	// HolderHuman: a named operator is driving.
	HolderHuman Holder = "human"
)

// State is the control token.
type State struct {
	Holder   Holder `json:"holder"`
	Operator string `json:"operator,omitempty"`
	// Epoch increments on every transfer. An actor that captured an older
	// epoch has lost control since, whatever it believes.
	Epoch int       `json:"epoch"`
	Since time.Time `json:"since"`
}

// ErrNotInControl is returned when an actor tries to act without the token.
var ErrNotInControl = errors.New("actor does not hold control of the session")

// Input is one raw action from a human operator.
type Input struct {
	Kind string  `json:"kind"` // click | type | key
	X    float64 `json:"x,omitempty"`
	Y    float64 `json:"y,omitempty"`
	Text string  `json:"text,omitempty"`
	Key  string  `json:"key,omitempty"`
}

// HumanAction is the record of one thing an operator did. Typed text is
// never recorded — an operator may be entering exactly the credential the
// automation is not allowed to have — only its length and where it went.
type HumanAction struct {
	At       string `json:"at"`
	Operator string `json:"operator"`
	Kind     string `json:"kind"`
	Element  string `json:"element,omitempty"`
	TextLen  int    `json:"text_len,omitempty"`
	Key      string `json:"key,omitempty"`
}

// Session is the one live session a run uses from start to finish, handoffs
// included.
type Session struct {
	surf surface.Surface
	run  *evidence.Run

	// OnTransfer, if set, is told of every change of control. It is called
	// while the transfer is in progress and must not call back into the Hub.
	OnTransfer func(State)

	// acting is held for the duration of any action and of any transfer, so
	// control never changes hands while input is still landing.
	acting sync.Mutex

	mu      sync.Mutex
	state   State
	actions []HumanAction
}

func NewSession(surf surface.Surface, run *evidence.Run) *Session {
	return &Session{surf: surf, run: run, state: State{Holder: HolderAutomation, Epoch: 1, Since: time.Now()}}
}

// Control reports who holds the session.
func (s *Session) Control() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) transfer(to Holder, operator, why string) State {
	s.acting.Lock()
	defer s.acting.Unlock()
	s.mu.Lock()
	from := s.state
	s.state = State{Holder: to, Operator: operator, Epoch: from.Epoch + 1, Since: time.Now()}
	now := s.state
	s.mu.Unlock()
	s.run.Log("system", "control.transfer", "", map[string]any{
		"from": from.Holder, "to": to, "operator": operator, "epoch": now.Epoch, "reason": why,
	})
	if s.OnTransfer != nil {
		s.OnTransfer(now)
	}
	return now
}

func (s *Session) require(h Holder, operator string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Holder != h || (h == HolderHuman && s.state.Operator != operator) {
		return fmt.Errorf("%w: session is held by %s %s", ErrNotInControl, s.state.Holder, s.state.Operator)
	}
	return nil
}

// ---- observation is always allowed: it changes nothing ----

func (s *Session) Open(ctx context.Context, url string) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	if err := s.require(HolderAutomation, ""); err != nil {
		return err
	}
	return s.surf.Open(ctx, url)
}

func (s *Session) Observe(ctx context.Context) (*surface.Observation, error) {
	return s.surf.Observe(ctx)
}

func (s *Session) Screenshot(ctx context.Context) ([]byte, error) {
	return s.surf.Screenshot(ctx)
}

// ---- automation's actions ----

func (s *Session) Click(ctx context.Context, el surface.Element) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	if err := s.require(HolderAutomation, ""); err != nil {
		return err
	}
	return s.surf.Click(ctx, el)
}

func (s *Session) Type(ctx context.Context, el surface.Element, text string) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	if err := s.require(HolderAutomation, ""); err != nil {
		return err
	}
	return s.surf.Type(ctx, el, text)
}

func (s *Session) Select(ctx context.Context, el surface.Element, option string) error {
	s.acting.Lock()
	defer s.acting.Unlock()
	if err := s.require(HolderAutomation, ""); err != nil {
		return err
	}
	return s.surf.Select(ctx, el, option)
}

// ---- the human's actions ----

// HumanInput applies one raw operator action to the live session and records
// what it touched.
func (s *Session) HumanInput(ctx context.Context, operator string, in Input) (HumanAction, error) {
	s.acting.Lock()
	defer s.acting.Unlock()
	if err := s.require(HolderHuman, operator); err != nil {
		return HumanAction{}, err
	}
	act := HumanAction{At: time.Now().UTC().Format(time.RFC3339), Operator: operator, Kind: in.Kind}
	var err error
	switch in.Kind {
	case "click":
		if obs, oerr := s.surf.Observe(ctx); oerr == nil {
			if el, ok := obs.At(in.X, in.Y); ok {
				name := el.Name
				if el.Protected || el.Role == surface.RoleTextbox {
					name = "" // a field's content is not a label
				}
				act.Element = fmt.Sprintf("%s %q in frame %s", el.Role, name, el.Frame)
			}
		}
		err = s.surf.Pointer(ctx, in.X, in.Y)
	case "type":
		act.TextLen = len(in.Text)
		err = s.surf.Keys(ctx, in.Text)
	case "key":
		act.Key = in.Key
		err = s.surf.Press(ctx, in.Key)
	default:
		return act, fmt.Errorf("unknown input kind %q", in.Kind)
	}
	if err != nil {
		return act, err
	}
	s.mu.Lock()
	s.actions = append(s.actions, act)
	s.mu.Unlock()
	s.run.Log("human:"+operator, "human.action", "", act)
	return act, nil
}

func (s *Session) takeActions() []HumanAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.actions
	s.actions = nil
	return out
}
