// Package locate identifies controls the way a person describes them — "the
// Search button", "the box next to Member Number", "the Current Balance of the
// Regular Savings row" — instead of by markup. Locators are pure functions of
// a surface.Observation, so they work unchanged on any surface that can
// report roles, names and rectangles, and they are trivially unit-testable.
package locate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/NightWatchEng/rote/internal/surface"
)

type Strategy string

const (
	// ByName: role + accessible name. Survives any layout change; breaks only
	// if the control is relabelled (which tenant label overrides absorb).
	ByName Strategy = "name"
	// ByLabel: role + the visible label beside or above it. This is what makes
	// unlabelled legacy form fields addressable at all.
	ByLabel Strategy = "label"
	// ByGrid: the intersection of a row anchor and a column header. Used to
	// read values out of tables without depending on row order.
	ByGrid Strategy = "grid"
	// ByOrdinal: the nth control of a role in a frame. Purely positional and
	// therefore fragile; it is a last resort, never used for irreversible
	// steps, and its use is always reported as drift.
	ByOrdinal Strategy = "ordinal"
)

// Locator is one way of finding a control.
type Locator struct {
	Strategy Strategy `json:"strategy"`
	Role     string   `json:"role"`
	Name     string   `json:"name,omitempty"`
	Label    string   `json:"label,omitempty"`
	Side     string   `json:"side,omitempty"` // where the label sits: "left" or "above"
	Row      string   `json:"row,omitempty"`
	Column   string   `json:"column,omitempty"`
	Index    int      `json:"index,omitempty"` // 1-based
}

// Target is an ordered list of locators for the same control, most robust
// first, scoped to a frame.
type Target struct {
	Frame     string    `json:"frame"`
	Locators  []Locator `json:"locators"`
	Rationale string    `json:"rationale,omitempty"`
}

// Match is a successful resolution. Rank > 0 means the primary locator no
// longer works and a fallback did: a drift signal worth reporting.
type Match struct {
	Element surface.Element
	Locator Locator
	Rank    int
	Misses  []string
}

// ErrNoMatch is returned when no locator resolves to exactly one element.
var ErrNoMatch = errors.New("no locator matched exactly one element")

func (l Locator) String() string {
	switch l.Strategy {
	case ByName:
		return fmt.Sprintf("%s named %q", l.Role, l.Name)
	case ByLabel:
		if l.Side == "above" {
			return fmt.Sprintf("%s below label %q", l.Role, l.Label)
		}
		return fmt.Sprintf("%s right of label %q", l.Role, l.Label)
	case ByGrid:
		return fmt.Sprintf("%s at row %q x column %q", l.Role, l.Row, l.Column)
	case ByOrdinal:
		return fmt.Sprintf("%s #%d", l.Role, l.Index)
	}
	return string(l.Strategy)
}

func (t Target) String() string {
	parts := make([]string, len(t.Locators))
	for i, l := range t.Locators {
		parts[i] = l.String()
	}
	return fmt.Sprintf("[%s] %s", t.Frame, strings.Join(parts, " | else "))
}

// Norm is the comparison form of visible text: case-folded, whitespace
// collapsed (including non-breaking spaces).
func Norm(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Resolve finds the control a target describes. Every locator must match
// exactly one element to count: an ambiguous match is a miss, because acting
// on a guess is worse than stopping. With strict set, positional locators are
// not considered at all.
func Resolve(t Target, obs *surface.Observation, strict bool) (Match, error) {
	els := inFrame(obs, t.Frame)
	var misses []string
	for rank, l := range t.Locators {
		if strict && l.Strategy == ByOrdinal {
			misses = append(misses, l.String()+": positional locator not allowed for this step")
			continue
		}
		el, err := resolveOne(l, els)
		if err != nil {
			misses = append(misses, l.String()+": "+err.Error())
			continue
		}
		return Match{Element: el, Locator: l, Rank: rank, Misses: misses}, nil
	}
	if len(els) == 0 {
		misses = append(misses, fmt.Sprintf("frame %q has no visible elements", t.Frame))
	}
	return Match{Misses: misses}, fmt.Errorf("%w: %s", ErrNoMatch, strings.Join(misses, "; "))
}

func inFrame(obs *surface.Observation, frame string) []surface.Element {
	var out []surface.Element
	for _, e := range obs.Elements {
		if e.Frame == frame {
			out = append(out, e)
		}
	}
	return out
}

func resolveOne(l Locator, els []surface.Element) (surface.Element, error) {
	switch l.Strategy {
	case ByName:
		var hits []surface.Element
		for _, e := range els {
			if e.Role == l.Role && Norm(e.Name) == Norm(l.Name) {
				hits = append(hits, e)
			}
		}
		return one(hits)
	case ByLabel:
		label, err := anchor(els, l.Label)
		if err != nil {
			return surface.Element{}, fmt.Errorf("label %w", err)
		}
		return nearest(els, l.Role, label, l.Side)
	case ByGrid:
		row, err := anchor(els, l.Row)
		if err != nil {
			return surface.Element{}, fmt.Errorf("row anchor %w", err)
		}
		col, err := anchor(els, l.Column)
		if err != nil {
			return surface.Element{}, fmt.Errorf("column anchor %w", err)
		}
		var hits []surface.Element
		for _, e := range els {
			if e.Role != l.Role || e.Ref == row.Ref || e.Ref == col.Ref {
				continue
			}
			if sameRow(e.Box(), row.Box()) && sameColumn(e.Box(), col.Box()) {
				hits = append(hits, e)
			}
		}
		return one(hits)
	case ByOrdinal:
		n := 0
		for _, e := range els {
			if e.Role == l.Role {
				n++
				if n == l.Index {
					return e, nil
				}
			}
		}
		return surface.Element{}, fmt.Errorf("only %d present", n)
	}
	return surface.Element{}, fmt.Errorf("unknown strategy %q", l.Strategy)
}

func one(hits []surface.Element) (surface.Element, error) {
	switch len(hits) {
	case 0:
		return surface.Element{}, errors.New("0 matches")
	case 1:
		return hits[0], nil
	}
	return surface.Element{}, fmt.Errorf("ambiguous (%d matches)", len(hits))
}

// anchor finds the single piece of visible text used as a reference point.
func anchor(els []surface.Element, text string) (surface.Element, error) {
	var hits []surface.Element
	for _, e := range els {
		if e.Role == surface.RoleText && Norm(e.Name) == Norm(text) {
			hits = append(hits, e)
		}
	}
	el, err := one(hits)
	if err != nil {
		return el, fmt.Errorf("%q: %w", text, err)
	}
	return el, nil
}

const (
	maxLabelGapX = 400.0 // a label further than this is not "next to" the control
	maxLabelGapY = 60.0
)

// Two boxes are in the same row (column) when they overlap by at least half
// of the larger one. Measuring against the larger box is what keeps a
// page-sized layout container from being "in the same row" as everything.
func sameRow(a, b surface.Rect) bool    { return a.OverlapY(b) >= 0.5*max(a.H, b.H) }
func sameColumn(a, b surface.Rect) bool { return a.OverlapX(b) >= 0.5*max(a.W, b.W) }

// nearest returns the control of the given role closest to a label on the
// stated side. Two equally close candidates are ambiguous.
func nearest(els []surface.Element, role string, label surface.Element, side string) (surface.Element, error) {
	const tie = 0.5
	var best surface.Element
	bestGap := -1.0
	var gaps []float64
	for _, e := range els {
		if e.Role != role || e.Ref == label.Ref {
			continue
		}
		gap, ok := labelGap(e.Bounds, label.Bounds, side)
		if !ok {
			continue
		}
		gaps = append(gaps, gap)
		if bestGap < 0 || gap < bestGap {
			best, bestGap = e, gap
		}
	}
	if len(gaps) == 0 {
		return surface.Element{}, fmt.Errorf("no %s %s label %q", role, sideWord(side), label.Name)
	}
	ties := 0
	for _, g := range gaps {
		if g <= bestGap+tie {
			ties++
		}
	}
	if ties > 1 {
		return surface.Element{}, fmt.Errorf("ambiguous (%d equally close)", ties)
	}
	return best, nil
}

func sideWord(side string) string {
	if side == "above" {
		return "below"
	}
	return "right of"
}

// labelGap is the distance from a label to a control on the given side, and
// whether the control is on that side at all.
func labelGap(ctl, label surface.Rect, side string) (float64, bool) {
	const slack = 2.0
	if side == "above" {
		gap := ctl.Y - (label.Y + label.H)
		return gap, ctl.OverlapX(label) > 0 && gap >= -slack && gap <= maxLabelGapY
	}
	gap := ctl.X - (label.X + label.W)
	return gap, sameRow(ctl, label) && gap >= -slack && gap <= maxLabelGapX
}
