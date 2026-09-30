// Package surface is the seam between "how we perceive and act on an
// application" and everything else (the recorded flow, replay, policy,
// handoff). A Surface turns whatever the application really is — a browser
// page, a frameset, a desktop window — into one normalized Observation: a flat
// list of what a human operator could see and touch, each with a role, a name
// and a rectangle. Nothing above this package knows about DOM, CDP or HTML.
package surface

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Normalized roles. Adapters map their native vocabulary onto these.
const (
	RoleText     = "text"
	RoleTextbox  = "textbox"
	RoleButton   = "button"
	RoleLink     = "link"
	RoleSelect   = "select"
	RoleCheckbox = "checkbox"
	RoleRadio    = "radio"
	RoleImage    = "image"
)

// DialogFrame is the frame name adapters use for a native modal dialog
// (a JavaScript confirm, a desktop message box). While one is open it is the
// only frame in the observation, because it is the only thing an operator
// could interact with.
const DialogFrame = "dialog"

// Rect is a rectangle in surface coordinates (CSS pixels of the top-level
// viewport for web; screen pixels for a desktop adapter).
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

func (r Rect) Center() (float64, float64) { return r.X + r.W/2, r.Y + r.H/2 }

func (r Rect) Contains(x, y float64) bool {
	return x >= r.X && x <= r.X+r.W && y >= r.Y && y <= r.Y+r.H
}

// OverlapX and OverlapY report how much two rectangles share along one axis.
func (r Rect) OverlapX(o Rect) float64 { return overlap(r.X, r.X+r.W, o.X, o.X+o.W) }
func (r Rect) OverlapY(o Rect) float64 { return overlap(r.Y, r.Y+r.H, o.Y, o.Y+o.H) }

func overlap(a1, a2, b1, b2 float64) float64 {
	lo, hi := max(a1, b1), min(a2, b2)
	if hi <= lo {
		return 0
	}
	return hi - lo
}

// Element is one visible thing on the surface.
type Element struct {
	// Ref is a handle valid only for the Observation it came from. It is never
	// persisted: artifacts identify controls with locators, not refs.
	Ref   int    `json:"ref"`
	Frame string `json:"frame"`
	Role  string `json:"role"`
	// Name is the accessible name for controls and the text itself for text.
	Name    string   `json:"name"`
	Value   string   `json:"value,omitempty"`
	Options []string `json:"options,omitempty"`
	// Protected marks controls whose content must never be read back
	// (password fields). Their Value is always masked by the adapter.
	Protected bool   `json:"protected,omitempty"`
	Href      string `json:"href,omitempty"`
	Bounds    Rect   `json:"bounds"`
	// Cell is the rectangle of the enclosing grid cell when the surface
	// exposes one. Text in a right-aligned column does not line up with its
	// header by its own bounds, but its cell does.
	Cell *Rect `json:"cell,omitempty"`
}

// Box is the rectangle used for row/column alignment.
func (e Element) Box() Rect {
	if e.Cell != nil {
		return *e.Cell
	}
	return e.Bounds
}

// Frame is one independently addressed region: an HTML frame, or a window.
type Frame struct {
	Name  string `json:"name"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

// Observation is a point-in-time snapshot of the whole surface.
type Observation struct {
	Seq      int       `json:"seq"`
	URL      string    `json:"url"`
	Frames   []Frame   `json:"frames"`
	Elements []Element `json:"elements"`
}

func (o *Observation) ByRef(ref int) (Element, bool) {
	for _, e := range o.Elements {
		if e.Ref == ref {
			return e, true
		}
	}
	return Element{}, false
}

func (o *Observation) FrameByName(name string) (Frame, bool) {
	for _, f := range o.Frames {
		if f.Name == name {
			return f, true
		}
	}
	return Frame{}, false
}

// At returns the smallest element whose bounds contain the point. It is how a
// human's raw click is translated back into "which control was that".
func (o *Observation) At(x, y float64) (Element, bool) {
	var best Element
	found := false
	for _, e := range o.Elements {
		if !e.Bounds.Contains(x, y) {
			continue
		}
		if !found || e.Bounds.W*e.Bounds.H < best.Bounds.W*best.Bounds.H {
			best, found = e, true
		}
	}
	return best, found
}

// Fingerprint changes whenever anything an operator could perceive changes.
// It is used to detect that the surface has settled, and that an action had
// no effect.
func (o *Observation) Fingerprint() string {
	h := sha256.New()
	for _, f := range o.Frames {
		fmt.Fprintf(h, "F|%s|%s|%s\n", f.Name, f.URL, f.Title)
	}
	for _, e := range o.Elements {
		fmt.Fprintf(h, "E|%s|%s|%s|%s|%s\n", e.Frame, e.Role, e.Name, e.Value, strings.Join(e.Options, ","))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Surface is what an adapter implements. Click/Type/Select take an Element
// from the most recent Observation; the adapter rejects stale ones.
//
// Pointer/Keys/Press are the raw input channel a human operator drives during
// a handoff: they bypass element resolution entirely, exactly as a person at
// the keyboard would.
type Surface interface {
	// Open starts a fresh application session at the entry point. Calling it
	// again discards the previous session state (cookies; for a desktop
	// adapter, the running instance).
	Open(ctx context.Context, url string) error
	Observe(ctx context.Context) (*Observation, error)

	Click(ctx context.Context, el Element) error
	Type(ctx context.Context, el Element, text string) error
	Select(ctx context.Context, el Element, option string) error

	Pointer(ctx context.Context, x, y float64) error
	Keys(ctx context.Context, text string) error
	Press(ctx context.Context, key string) error

	Screenshot(ctx context.Context) ([]byte, error)
	Close() error
}

// Observer is the read-only half of a Surface.
type Observer interface {
	Observe(ctx context.Context) (*Observation, error)
}

// Settle observes until two consecutive snapshots are identical, or until
// limit has passed, and returns the last snapshot. It answers "has the screen
// stopped changing?" without knowing anything about page loads.
func Settle(ctx context.Context, o Observer, limit time.Duration) (*Observation, error) {
	deadline := time.Now().Add(limit)
	prev, err := o.Observe(ctx)
	if err != nil {
		return nil, err
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return prev, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		cur, err := o.Observe(ctx)
		if err != nil {
			return nil, err
		}
		if cur.Fingerprint() == prev.Fingerprint() {
			return cur, nil
		}
		prev = cur
	}
	return prev, nil
}
