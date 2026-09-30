// Package fake is an in-memory surface.Surface: a small scripted application
// made of named screens, used to test the discovery agent and the replay
// engine without a browser. It is also the proof that a second surface can
// sit behind the same interface as the web adapter — nothing above package
// surface can tell the two apart.
//
// A screen is a list of frames and elements. Acting on a control whose name
// appears in the screen's Next map moves the application to that screen;
// anything more particular ("the second click on Search shows an error") is
// scripted with a Hook.
package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/NightWatchEng/rote/internal/surface"
)

// Screen is one state of the application.
type Screen struct {
	// Frames hold a path in URL; the application prefixes its base URL.
	Frames []surface.Frame
	// Elements are copied fresh every time the screen is shown, so typed
	// values do not survive leaving it. Ref is ignored; an empty Frame means
	// the first frame.
	Elements []surface.Element
	// Next maps a control name to the screen that acting on it leads to.
	Next map[string]string
}

// Action is one call the application received.
type Action struct {
	Kind   string // open | click | type | select | pointer | keys | press
	Screen string // the screen showing when it arrived
	Role   string // the control acted on; empty for raw input that hit nothing
	Name   string
	Text   string // typed text, chosen option, key, or the URL opened
	X, Y   float64
	// N counts the actions of this kind this control has received, this one
	// included: N == 2 on the second click on the same button.
	N int
}

// Effect is what a hook may change about an action before it is applied.
type Effect struct {
	Text    string // what lands in the field (type, select, keys)
	Show    string // the screen to show instead of the default transition
	Swallow bool   // the action has no effect at all
}

// Hook reacts to an action. It runs without the application locked, so it
// may call Show directly or later (time.AfterFunc) to simulate a slow screen.
type Hook func(a Action, e *Effect)

// App is the scripted application.
type App struct {
	base, entry string
	screens     map[string]Screen

	mu      sync.Mutex
	hooks   []Hook
	cur     string
	live    []surface.Element
	frames  []surface.Frame
	refs    map[int]int // ref -> index into live, for the latest observation only
	nextRef int
	seq     int
	focus   int
	counts  map[string]int
	actions []Action
}

var _ surface.Surface = (*App)(nil)

// ErrStale mirrors the web adapter: a ref is valid only for the observation
// it came from.
var ErrStale = errors.New("element is from an older observation; observe again before acting")

// New builds an application at base (e.g. "http://bank.test") whose sessions
// start on the entry screen.
func New(base, entry string, screens map[string]Screen) *App {
	if _, ok := screens[entry]; !ok {
		panic("fake: unknown entry screen " + entry)
	}
	a := &App{base: strings.TrimSuffix(base, "/"), entry: entry, screens: screens, counts: map[string]int{}, nextRef: 1}
	a.show(entry)
	return a
}

// OnAction adds a hook. Hooks run in the order they were added.
func (a *App) OnAction(h Hook) {
	a.mu.Lock()
	a.hooks = append(a.hooks, h)
	a.mu.Unlock()
}

// Show moves the application to a screen, as if it had navigated by itself.
func (a *App) Show(screen string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.show(screen)
}

func (a *App) show(name string) {
	sc, ok := a.screens[name]
	if !ok {
		panic("fake: unknown screen " + name)
	}
	a.cur, a.focus, a.refs = name, -1, map[int]int{}
	a.frames = slices.Clone(sc.Frames)
	a.live = make([]surface.Element, len(sc.Elements))
	for i, e := range sc.Elements {
		if e.Frame == "" && len(sc.Frames) > 0 {
			e.Frame = sc.Frames[0].Name
		}
		e.Options = slices.Clone(e.Options)
		a.live[i] = e
	}
}

// Current names the screen showing now.
func (a *App) Current() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur
}

// Actions returns every action received, in order.
func (a *App) Actions() []Action {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.actions)
}

// Count reports how many actions of a kind a named control received
// (for "open", name is ignored).
func (a *App) Count(kind, name string) int {
	n := 0
	for _, act := range a.Actions() {
		if act.Kind == kind && (kind == "open" || act.Name == name) {
			n++
		}
	}
	return n
}

// ---- surface.Surface ----

func (a *App) Open(_ context.Context, url string) error {
	act, hooks := a.receive(Action{Kind: "open", Text: url})
	eff := runHooks(hooks, act, "")
	a.mu.Lock()
	defer a.mu.Unlock()
	a.show(a.entry)
	if eff.Show != "" {
		a.show(eff.Show)
	}
	return nil
}

func (a *App) Observe(context.Context) (*surface.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	obs := &surface.Observation{Seq: a.seq}
	for _, f := range a.frames {
		f.URL = a.base + f.URL
		obs.Frames = append(obs.Frames, f)
	}
	if len(obs.Frames) > 0 {
		obs.URL = obs.Frames[0].URL
	}
	a.refs = map[int]int{}
	for i, e := range a.live {
		e.Ref = a.nextRef
		a.nextRef++
		a.refs[e.Ref] = i
		e.Options = slices.Clone(e.Options)
		if e.Protected && e.Value != "" {
			e.Value = strings.Repeat("•", utf8.RuneCountInString(e.Value))
		}
		obs.Elements = append(obs.Elements, e)
	}
	return obs, nil
}

func (a *App) Click(_ context.Context, el surface.Element) error {
	return a.act("click", el, "")
}

func (a *App) Type(_ context.Context, el surface.Element, text string) error {
	return a.act("type", el, text)
}

func (a *App) Select(_ context.Context, el surface.Element, option string) error {
	return a.act("select", el, option)
}

func (a *App) Pointer(_ context.Context, x, y float64) error {
	a.mu.Lock()
	idx := -1
	for i, e := range a.live {
		// The smallest element under the point, as a real click would hit.
		if e.Bounds.Contains(x, y) && (idx < 0 || e.Bounds.W*e.Bounds.H < a.live[idx].Bounds.W*a.live[idx].Bounds.H) {
			idx = i
		}
	}
	act := Action{Kind: "pointer", X: x, Y: y}
	if idx >= 0 {
		act.Role, act.Name = a.live[idx].Role, a.live[idx].Name
	}
	a.mu.Unlock()
	act, hooks := a.receive(act)
	eff := runHooks(hooks, act, "")
	a.mu.Lock()
	defer a.mu.Unlock()
	if eff.Swallow || a.cur != act.Screen {
		return nil
	}
	if idx >= 0 {
		a.focus = idx
	}
	a.transition(act.Name, eff.Show)
	return nil
}

func (a *App) Keys(_ context.Context, text string) error {
	act, hooks := a.receive(Action{Kind: "keys", Text: text})
	eff := runHooks(hooks, act, text)
	a.mu.Lock()
	defer a.mu.Unlock()
	if eff.Swallow || a.cur != act.Screen {
		return nil
	}
	if a.focus >= 0 && a.live[a.focus].Role == surface.RoleTextbox {
		a.live[a.focus].Value += eff.Text
	}
	a.transition("", eff.Show)
	return nil
}

func (a *App) Press(_ context.Context, key string) error {
	act, hooks := a.receive(Action{Kind: "press", Text: key})
	eff := runHooks(hooks, act, "")
	a.mu.Lock()
	defer a.mu.Unlock()
	if !eff.Swallow && a.cur == act.Screen {
		a.transition("", eff.Show)
	}
	return nil
}

// Screenshot is a blank image: evidence code can store and redact it.
func (a *App) Screenshot(context.Context) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8))); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *App) Close() error { return nil }

// ---- internals ----

// act applies a click, type or select to an element from the latest
// observation.
func (a *App) act(kind string, el surface.Element, text string) error {
	a.mu.Lock()
	idx, ok := a.refs[el.Ref]
	if !ok {
		a.mu.Unlock()
		return ErrStale
	}
	live := a.live[idx]
	switch kind {
	case "type":
		if live.Role != surface.RoleTextbox {
			a.mu.Unlock()
			return fmt.Errorf("cannot type into a %s", live.Role)
		}
	case "select":
		if !slices.Contains(live.Options, text) {
			a.mu.Unlock()
			return fmt.Errorf("%q is not an option of %q", text, live.Name)
		}
	}
	a.mu.Unlock()

	act, hooks := a.receive(Action{Kind: kind, Role: live.Role, Name: live.Name, Text: text})
	eff := runHooks(hooks, act, text)

	a.mu.Lock()
	defer a.mu.Unlock()
	// A hook that already moved the application elsewhere has decided what
	// this action led to.
	if eff.Swallow || a.cur != act.Screen {
		return nil
	}
	if kind != "click" {
		a.live[idx].Value = eff.Text
	}
	a.focus = idx
	a.transition(live.Name, eff.Show)
	return nil
}

// receive records an action and returns it with the hooks to run.
func (a *App) receive(act Action) (Action, []Hook) {
	a.mu.Lock()
	defer a.mu.Unlock()
	act.Screen = a.cur
	key := act.Kind + "|" + act.Name
	a.counts[key]++
	act.N = a.counts[key]
	a.actions = append(a.actions, act)
	return act, slices.Clone(a.hooks)
}

func runHooks(hooks []Hook, act Action, text string) Effect {
	eff := Effect{Text: text}
	for _, h := range hooks {
		h(act, &eff)
	}
	return eff
}

func (a *App) transition(control, show string) {
	if show == "" {
		show = a.screens[a.cur].Next[control]
	}
	if show != "" {
		a.show(show)
	}
}
