package locate

import (
	"strings"
	"testing"

	"github.com/NightWatchEng/rote/internal/surface"
)

func strategies(t Target) []Strategy {
	out := make([]Strategy, len(t.Locators))
	for i, l := range t.Locators {
		out[i] = l.Strategy
	}
	return out
}

func sameStrategies(a, b []Strategy) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDerive(t *testing.T) {
	cases := []struct {
		name string
		ref  int
		want []Strategy
		// label is the expected label text when a label locator is derived.
		label, side string
	}{
		{name: "unnamed textbox gets its left label", ref: 4, want: []Strategy{ByLabel, ByOrdinal}, label: "Member Number:", side: "left"},
		{name: "second textbox gets its own label", ref: 8, want: []Strategy{ByLabel, ByOrdinal}, label: "Nickname:", side: "left"},
		{name: "select gets the label above", ref: 10, want: []Strategy{ByLabel, ByOrdinal}, label: "Branch", side: "above"},
		// "Member Number:" also sits left of the button and would resolve to
		// it, but text beside a button is not what identifies it.
		{name: "button uses name, never label", ref: 5, want: []Strategy{ByName, ByOrdinal}},
		{name: "duplicate name is dropped", ref: 28, want: []Strategy{ByOrdinal}},
		{name: "nav text by name", ref: 41, want: []Strategy{ByName, ByOrdinal}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := memberDetail()
			e := el(t, obs, c.ref)
			tgt, err := Derive(obs, e)
			if err != nil {
				t.Fatal(err)
			}
			if tgt.Frame != e.Frame {
				t.Fatalf("frame %q, want %q", tgt.Frame, e.Frame)
			}
			if got := strategies(tgt); !sameStrategies(got, c.want) {
				t.Fatalf("strategies %v, want %v", got, c.want)
			}
			if c.label != "" {
				l := tgt.Locators[0]
				if l.Label != c.label || l.Side != c.side {
					t.Fatalf("label %q side %q, want %q %q", l.Label, l.Side, c.label, c.side)
				}
			}
			if tgt.Rationale == "" {
				t.Fatal("a derived target explains itself")
			}
		})
	}
}

// Every locator Derive proposes must, on its own, find the element again.
func TestDeriveRoundTrips(t *testing.T) {
	obs := memberDetail()
	for _, e := range obs.Elements {
		if e.Role == surface.RoleText && e.Name == "" {
			continue
		}
		tgt, err := Derive(obs, e)
		if err != nil {
			t.Errorf("ref %d: %v", e.Ref, err)
			continue
		}
		last := tgt.Locators[len(tgt.Locators)-1]
		if last.Strategy != ByOrdinal {
			t.Errorf("ref %d: ordinal must be the last resort, got %v", e.Ref, strategies(tgt))
		}
		for _, l := range tgt.Locators {
			m, err := Resolve(Target{Frame: tgt.Frame, Locators: []Locator{l}}, obs, false)
			if err != nil || m.Element.Ref != e.Ref {
				t.Errorf("ref %d: %v resolves to %d (%v)", e.Ref, l, m.Element.Ref, err)
			}
		}
	}
}

func TestDeriveNothingToAnchor(t *testing.T) {
	obs := memberDetail()
	ghost := ctl(99, "main", surface.RoleButton, "", rect(700, 700, 10, 10))
	if _, err := Derive(obs, ghost); err == nil {
		t.Fatal("an unnamed element that is not in the observation has no locator")
	}
}

func TestDeriveValue(t *testing.T) {
	cases := []struct {
		name        string
		ref         int
		row, column string
		want        Locator
		fails       string
	}{
		{name: "grid cell", ref: 24, row: "Regular Savings", column: "Current Balance",
			want: Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Regular Savings", Column: "Current Balance"}},
		{name: "grid names another cell", ref: 24, row: "Share Draft", column: "Current Balance",
			fails: "identifies [27]"},
		{name: "grid anchor missing", ref: 24, row: "Christmas Club", column: "Current Balance",
			fails: "does not identify the element"},
		{name: "grid with only a row", ref: 24, row: "Regular Savings",
			fails: "does not identify the element"},
		{name: "adjacent label", ref: 12,
			want: Locator{Strategy: ByLabel, Role: surface.RoleText, Label: "Available:", Side: "left"}},
		{name: "nothing anchors it", ref: 31, fails: "no adjacent label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := memberDetail()
			tgt, err := DeriveValue(obs, el(t, obs, c.ref), c.row, c.column)
			if c.fails != "" {
				if err == nil || !strings.Contains(err.Error(), c.fails) {
					t.Fatalf("want error mentioning %q, got %v (target %v)", c.fails, err, tgt)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Exactly one locator: never the value's own text, never position.
			if len(tgt.Locators) != 1 || tgt.Locators[0] != c.want {
				t.Fatalf("locators %v, want [%v]", tgt.Locators, c.want)
			}
			m, err := Resolve(tgt, obs, true)
			if err != nil || m.Element.Ref != c.ref {
				t.Fatalf("derived target resolves to %d (%v)", m.Element.Ref, err)
			}
		})
	}
}

// The recorded value locator must keep working when the value changes and
// the rows are reordered — the reason values are anchored rather than named.
func TestDeriveValueSurvivesNewDataAndRowOrder(t *testing.T) {
	obs := memberDetail()
	tgt, err := DeriveValue(obs, el(t, obs, 24), "Regular Savings", "Current Balance")
	if err != nil {
		t.Fatal(err)
	}
	next := memberDetail()
	for i := range next.Elements {
		e := &next.Elements[i]
		switch e.Name {
		case "Regular Savings":
			e.Bounds.Y, e.Cell.Y = 340, 338
		case "$5,230.18":
			e.Name, e.Bounds.Y, e.Cell.Y = "$9,999.99", 340, 338
		case "Holiday Club":
			e.Bounds.Y, e.Cell.Y = 280, 278
		case "-$3.50":
			e.Bounds.Y, e.Cell.Y = 280, 278
		}
	}
	m, err := Resolve(tgt, next, true)
	if err != nil || m.Element.Name != "$9,999.99" {
		t.Fatalf("got %q (%v), want the moved and changed balance", m.Element.Name, err)
	}
}
