package locate

import (
	"fmt"
	"strings"

	"github.com/NightWatchEng/rote/internal/surface"
)

// Derive builds the target for a control the agent just chose to act on. It
// proposes locators from most to least robust and keeps only those that
// provably resolve back to that same element in the current observation, so
// a recorded locator is never a guess.
func Derive(obs *surface.Observation, el surface.Element) (Target, error) {
	els := inFrame(obs, el.Frame)
	t := Target{Frame: el.Frame}
	var why []string

	if el.Name != "" {
		l := Locator{Strategy: ByName, Role: el.Role, Name: el.Name}
		if verify(l, els, el) {
			t.Locators = append(t.Locators, l)
			why = append(why, "its own name is unique in the frame")
		}
	}
	// Only form fields are identified by a neighbouring label; for a button
	// or link, whatever text happens to sit beside it means nothing.
	if l, ok := byLabel(els, el); ok && formField[el.Role] {
		t.Locators = append(t.Locators, l)
		why = append(why, fmt.Sprintf("the visible label %q sits %s it", l.Label, map[string]string{"left": "left of", "above": "above"}[l.Side]))
	}
	if idx := ordinal(els, el); idx > 0 {
		t.Locators = append(t.Locators, Locator{Strategy: ByOrdinal, Role: el.Role, Index: idx})
		why = append(why, "position in frame (fragile; last resort, reported as drift if used)")
	}
	if len(t.Locators) == 0 {
		return t, fmt.Errorf("no locator could be derived for %s %q", el.Role, el.Name)
	}
	t.Rationale = strings.Join(why, "; ")
	return t, nil
}

// DeriveValue builds the target for a value to read. Values are never located
// by their own text (that is the data) or by position (any content change
// shifts it), only by what they are anchored to: a row and a column header in
// a table, or an adjacent label.
func DeriveValue(obs *surface.Observation, el surface.Element, row, column string) (Target, error) {
	els := inFrame(obs, el.Frame)
	t := Target{Frame: el.Frame}
	if row != "" || column != "" {
		l := Locator{Strategy: ByGrid, Role: el.Role, Row: row, Column: column}
		got, err := resolveOne(l, els)
		if err != nil {
			return t, fmt.Errorf("row %q x column %q does not identify the element: %v", row, column, err)
		}
		if got.Ref != el.Ref {
			return t, fmt.Errorf("row %q x column %q identifies [%d] %q, not the element you chose", row, column, got.Ref, got.Name)
		}
		t.Locators = []Locator{l}
		t.Rationale = "table cell at a named row and column header; independent of row order and of the value itself"
		return t, nil
	}
	if l, ok := byLabel(els, el); ok {
		t.Locators = []Locator{l}
		t.Rationale = fmt.Sprintf("value adjacent to the label %q; independent of the value itself", l.Label)
		return t, nil
	}
	return t, fmt.Errorf("the value has no adjacent label; give row_anchor and column_anchor text that identify its table cell")
}

var formField = map[string]bool{
	surface.RoleTextbox: true, surface.RoleSelect: true, surface.RoleCheckbox: true, surface.RoleRadio: true,
}

func verify(l Locator, els []surface.Element, want surface.Element) bool {
	got, err := resolveOne(l, els)
	return err == nil && got.Ref == want.Ref
}

// byLabel looks for the closest text to the left of, then above, the element
// and accepts it only if the resulting locator resolves back to the element.
func byLabel(els []surface.Element, el surface.Element) (Locator, bool) {
	for _, side := range []string{"left", "above"} {
		var label surface.Element
		best := -1.0
		for _, c := range els {
			if c.Role != surface.RoleText || c.Ref == el.Ref || strings.TrimSpace(c.Name) == "" {
				continue
			}
			gap, ok := labelGap(el.Bounds, c.Bounds, side)
			if ok && (best < 0 || gap < best) {
				label, best = c, gap
			}
		}
		if best < 0 {
			continue
		}
		l := Locator{Strategy: ByLabel, Role: el.Role, Label: label.Name, Side: side}
		if verify(l, els, el) {
			return l, true
		}
	}
	return Locator{}, false
}

func ordinal(els []surface.Element, el surface.Element) int {
	n := 0
	for _, e := range els {
		if e.Role == el.Role {
			n++
			if e.Ref == el.Ref {
				return n
			}
		}
	}
	return 0
}
