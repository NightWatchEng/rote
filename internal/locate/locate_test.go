package locate

import (
	"errors"
	"strings"
	"testing"

	"github.com/NightWatchEng/rote/internal/surface"
)

func rect(x, y, w, h float64) surface.Rect { return surface.Rect{X: x, Y: y, W: w, H: h} }

func cell(x, y, w, h float64) *surface.Rect { r := rect(x, y, w, h); return &r }

func text(ref int, frame, name string, b surface.Rect) surface.Element {
	return surface.Element{Ref: ref, Frame: frame, Role: surface.RoleText, Name: name, Bounds: b}
}

func textIn(ref int, name string, b surface.Rect, c *surface.Rect) surface.Element {
	e := text(ref, "main", name, b)
	e.Cell = c
	return e
}

func ctl(ref int, frame, role, name string, b surface.Rect) surface.Element {
	return surface.Element{Ref: ref, Frame: frame, Role: role, Name: name, Bounds: b}
}

// memberDetail is a legacy member screen: a label-left form, a field with its
// label above, and a share-accounts table whose amounts are right-aligned
// inside their cells (so only the cell lines up with the column header). The
// table sits inside a text container spanning every row and column.
func memberDetail() *surface.Observation {
	return &surface.Observation{
		URL: "http://127.0.0.1:8080/",
		Frames: []surface.Frame{
			{Name: "main", URL: "http://127.0.0.1:8080/member?no=10042", Title: "Member Detail"},
			{Name: "nav", URL: "http://127.0.0.1:8080/nav", Title: "Navigation"},
			{Name: "empty", URL: "about:blank"},
		},
		Elements: []surface.Element{
			text(1, "main", "Member Detail", rect(20, 10, 200, 30)),
			text(2, "main", "Member Detail loaded for member 10042", rect(20, 50, 400, 20)),

			text(3, "main", "Member Number:", rect(20, 100, 110, 20)),
			ctl(4, "main", surface.RoleTextbox, "", rect(140, 98, 160, 24)),
			ctl(5, "main", surface.RoleButton, "Search", rect(320, 98, 80, 24)),
			ctl(6, "main", surface.RoleButton, "Clear", rect(410, 98, 80, 24)),
			text(7, "main", "Nickname:", rect(20, 130, 80, 20)),
			ctl(8, "main", surface.RoleTextbox, "", rect(140, 128, 160, 24)),
			text(9, "main", "Branch", rect(20, 160, 60, 20)),
			ctl(10, "main", surface.RoleSelect, "", rect(20, 184, 160, 24)),
			text(11, "main", "Available:", rect(20, 214, 80, 20)),
			text(12, "main", "$5,100.00", rect(140, 214, 70, 20)),

			textIn(20, "Share Accounts", rect(0, 240, 800, 140), nil),
			textIn(21, "Account", rect(20, 250, 60, 20), cell(20, 248, 200, 26)),
			textIn(22, "Current Balance", rect(230, 250, 110, 20), cell(220, 248, 150, 26)),
			textIn(23, "Regular Savings", rect(20, 280, 110, 20), cell(20, 278, 200, 26)),
			textIn(24, "$5,230.18", rect(300, 280, 65, 20), cell(220, 278, 150, 26)),
			ctl(25, "main", surface.RoleLink, "Details", rect(380, 280, 50, 20)),
			textIn(26, "Share Draft", rect(20, 310, 80, 20), cell(20, 308, 200, 26)),
			textIn(27, "$12.00", rect(320, 310, 45, 20), cell(220, 308, 150, 26)),
			ctl(28, "main", surface.RoleLink, "Details", rect(380, 310, 50, 20)),
			textIn(29, "Holiday Club", rect(20, 340, 90, 20), cell(20, 338, 200, 26)),
			textIn(30, "-$3.50", rect(325, 340, 40, 20), cell(220, 338, 150, 26)),

			text(31, "main", "Orphan note", rect(600, 500, 80, 20)),

			text(40, "nav", "Home", rect(5, 10, 80, 20)),
			text(41, "nav", "Member Inquiry", rect(5, 40, 110, 20)),
			ctl(42, "nav", surface.RoleButton, "Search", rect(5, 70, 80, 24)),
		},
	}
}

func el(t *testing.T, obs *surface.Observation, ref int) surface.Element {
	t.Helper()
	e, ok := obs.ByRef(ref)
	if !ok {
		t.Fatalf("fixture has no element %d", ref)
	}
	return e
}

func target(frame string, ls ...Locator) Target { return Target{Frame: frame, Locators: ls} }

func TestResolveStrategies(t *testing.T) {
	obs := memberDetail()
	cases := []struct {
		name  string
		t     Target
		want  int
		fails string // substring of the error when the target must not resolve
	}{
		{name: "name", t: target("main", Locator{Strategy: ByName, Role: surface.RoleButton, Name: "Search"}), want: 5},
		{name: "name is normalized", t: target("main", Locator{Strategy: ByName, Role: surface.RoleButton, Name: "  search "}), want: 5},
		{name: "name must match role", t: target("main", Locator{Strategy: ByName, Role: surface.RoleLink, Name: "Search"}), fails: "0 matches"},
		{name: "name with no match", t: target("main", Locator{Strategy: ByName, Role: surface.RoleButton, Name: "Delete"}), fails: "0 matches"},
		{name: "name ambiguous", t: target("main", Locator{Strategy: ByName, Role: surface.RoleLink, Name: "Details"}), fails: "ambiguous (2 matches)"},

		{name: "label left", t: target("main", Locator{Strategy: ByLabel, Role: surface.RoleTextbox, Label: "Member Number:", Side: "left"}), want: 4},
		{name: "label left, second row", t: target("main", Locator{Strategy: ByLabel, Role: surface.RoleTextbox, Label: "Nickname:", Side: "left"}), want: 8},
		{name: "label left picks the nearest", t: target("main", Locator{Strategy: ByLabel, Role: surface.RoleButton, Label: "Member Number:", Side: "left"}), want: 5},
		{name: "label above", t: target("main", Locator{Strategy: ByLabel, Role: surface.RoleSelect, Label: "Branch", Side: "above"}), want: 10},
		{name: "label on the wrong side", t: target("main", Locator{Strategy: ByLabel, Role: surface.RoleSelect, Label: "Branch", Side: "left"}), fails: "no select right of label"},
		{name: "label text missing", t: target("main", Locator{Strategy: ByLabel, Role: surface.RoleTextbox, Label: "SSN/TIN:", Side: "left"}), fails: "label \"SSN/TIN:\": 0 matches"},

		{name: "grid by cell", t: target("main", Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Regular Savings", Column: "Current Balance"}), want: 24},
		{name: "grid other row", t: target("main", Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Holiday Club", Column: "Current Balance"}), want: 30},
		{name: "grid with no cell at the intersection", t: target("main", Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Share Draft", Column: "Account"}), fails: "0 matches"},
		{name: "grid missing row anchor", t: target("main", Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Christmas Club", Column: "Current Balance"}), fails: "row anchor"},
		{name: "grid missing column anchor", t: target("main", Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Regular Savings", Column: "Available Balance"}), fails: "column anchor"},

		{name: "ordinal", t: target("main", Locator{Strategy: ByOrdinal, Role: surface.RoleTextbox, Index: 2}), want: 8},
		{name: "ordinal out of range", t: target("main", Locator{Strategy: ByOrdinal, Role: surface.RoleTextbox, Index: 3}), fails: "only 2 present"},

		{name: "unknown strategy", t: target("main", Locator{Strategy: "xpath", Role: surface.RoleButton}), fails: "unknown strategy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := Resolve(c.t, obs, false)
			if c.fails != "" {
				if !errors.Is(err, ErrNoMatch) {
					t.Fatalf("want ErrNoMatch, got %v (matched ref %d)", err, m.Element.Ref)
				}
				if !strings.Contains(err.Error(), c.fails) {
					t.Fatalf("error %q does not mention %q", err, c.fails)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Element.Ref != c.want || m.Rank != 0 || len(m.Misses) != 0 {
				t.Fatalf("got ref %d rank %d misses %v, want ref %d at rank 0", m.Element.Ref, m.Rank, m.Misses, c.want)
			}
		})
	}
}

func TestResolveLabelTieIsAmbiguous(t *testing.T) {
	// A tall label with two stacked boxes beside it: both are "in its row" and
	// equally far away, so neither may be picked.
	obs := &surface.Observation{Elements: []surface.Element{
		text(1, "main", "Amount:", rect(20, 100, 60, 40)),
		ctl(2, "main", surface.RoleTextbox, "", rect(100, 100, 100, 20)),
		ctl(3, "main", surface.RoleTextbox, "", rect(100, 120, 100, 20)),
	}}
	_, err := Resolve(target("main", Locator{Strategy: ByLabel, Role: surface.RoleTextbox, Label: "Amount:", Side: "left"}), obs, false)
	if err == nil || !strings.Contains(err.Error(), "ambiguous (2 equally close)") {
		t.Fatalf("want an ambiguity miss, got %v", err)
	}
}

func TestResolveLabelTooFar(t *testing.T) {
	obs := &surface.Observation{Elements: []surface.Element{
		text(1, "main", "Amount:", rect(20, 100, 60, 20)),
		ctl(2, "main", surface.RoleTextbox, "", rect(20+60+maxLabelGapX+10, 100, 100, 20)),
	}}
	if _, err := Resolve(target("main", Locator{Strategy: ByLabel, Role: surface.RoleTextbox, Label: "Amount:", Side: "left"}), obs, false); err == nil {
		t.Fatal("a box far across the page is not next to the label")
	}
}

func TestResolveFallback(t *testing.T) {
	obs := memberDetail()
	renamed := Locator{Strategy: ByName, Role: surface.RoleButton, Name: "Find"}
	byPosition := Locator{Strategy: ByOrdinal, Role: surface.RoleButton, Index: 1}

	m, err := Resolve(target("main", renamed, byPosition), obs, false)
	if err != nil {
		t.Fatal(err)
	}
	if m.Element.Ref != 5 || m.Rank != 1 || m.Locator != byPosition {
		t.Fatalf("got ref %d rank %d via %v, want ref 5 at rank 1 via ordinal", m.Element.Ref, m.Rank, m.Locator)
	}
	if len(m.Misses) != 1 || !strings.Contains(m.Misses[0], `"Find"`) {
		t.Fatalf("misses should record the failed primary locator, got %v", m.Misses)
	}

	// The first locator that matches wins even when a later one would too.
	m, err = Resolve(target("main", Locator{Strategy: ByName, Role: surface.RoleButton, Name: "Clear"}, byPosition), obs, false)
	if err != nil || m.Element.Ref != 6 || m.Rank != 0 {
		t.Fatalf("got ref %d rank %d err %v, want ref 6 at rank 0", m.Element.Ref, m.Rank, err)
	}
}

func TestResolveStrictRefusesOrdinal(t *testing.T) {
	obs := memberDetail()
	renamed := Locator{Strategy: ByName, Role: surface.RoleButton, Name: "Find"}
	byPosition := Locator{Strategy: ByOrdinal, Role: surface.RoleButton, Index: 1}

	m, err := Resolve(target("main", renamed, byPosition), obs, true)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("strict resolution fell back to a positional locator: ref %d", m.Element.Ref)
	}
	if len(m.Misses) != 2 || !strings.Contains(m.Misses[1], "positional locator not allowed") {
		t.Fatalf("misses = %v", m.Misses)
	}

	// Strict still accepts every non-positional strategy.
	if _, err := Resolve(target("main", Locator{Strategy: ByName, Role: surface.RoleButton, Name: "Search"}), obs, true); err != nil {
		t.Fatal(err)
	}
}

func TestResolveFrameScoping(t *testing.T) {
	obs := memberDetail()
	search := Locator{Strategy: ByName, Role: surface.RoleButton, Name: "Search"}

	for frame, want := range map[string]int{"main": 5, "nav": 42} {
		m, err := Resolve(target(frame, search), obs, false)
		if err != nil || m.Element.Ref != want {
			t.Fatalf("frame %s: got ref %d err %v, want %d", frame, m.Element.Ref, err, want)
		}
	}
	if _, err := Resolve(target("main", Locator{Strategy: ByName, Role: surface.RoleText, Name: "Member Inquiry"}), obs, false); err == nil {
		t.Fatal("a nav link must not be found in the main frame")
	}
	_, err := Resolve(target("empty", search), obs, false)
	if err == nil || !strings.Contains(err.Error(), `frame "empty" has no visible elements`) {
		t.Fatalf("want an empty-frame explanation, got %v", err)
	}
}

func TestAlignmentAgainstLargerBox(t *testing.T) {
	page := rect(0, 0, 1024, 768)
	small := rect(220, 278, 150, 26)
	if sameRow(page, small) || sameRow(small, page) {
		t.Fatal("a page-sized container is not in the same row as a cell")
	}
	if sameColumn(page, small) || sameColumn(small, page) {
		t.Fatal("a page-sized container is not in the same column as a cell")
	}
	// Two cells of one row overlap fully in Y even at different widths.
	if !sameRow(rect(20, 278, 200, 26), small) {
		t.Fatal("cells of one row must align")
	}
	// Half a row's height of overlap is enough; less is a neighbouring row.
	if !sameRow(rect(0, 0, 10, 20), rect(50, 10, 10, 20)) || sameRow(rect(0, 0, 10, 20), rect(50, 11, 10, 20)) {
		t.Fatal("row threshold is half of the larger height")
	}
}

func TestGridIgnoresSpanningContainer(t *testing.T) {
	// The fixture's "Share Accounts" container spans every row and column;
	// if it counted as aligned, every grid lookup would be ambiguous.
	obs := memberDetail()
	m, err := Resolve(target("main", Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Share Draft", Column: "Current Balance"}), obs, false)
	if err != nil || m.Element.Ref != 27 {
		t.Fatalf("got ref %d err %v, want the Share Draft balance", m.Element.Ref, err)
	}
}

func TestGridUsesCellNotTextBounds(t *testing.T) {
	obs := memberDetail()
	loc := Locator{Strategy: ByGrid, Role: surface.RoleText, Row: "Regular Savings", Column: "Current Balance"}
	if _, err := Resolve(target("main", loc), obs, false); err != nil {
		t.Fatal(err)
	}
	for i := range obs.Elements {
		obs.Elements[i].Cell = nil
	}
	// By their own bounds, the right-aligned amount and its header barely
	// overlap horizontally, so without cells the column does not line up.
	if _, err := Resolve(target("main", loc), obs, false); err == nil {
		t.Fatal("without cell rectangles the right-aligned amount should not align with its header")
	}
}

func TestElementBox(t *testing.T) {
	e := text(1, "main", "x", rect(1, 2, 3, 4))
	if e.Box() != e.Bounds {
		t.Fatal("Box without a cell is the bounds")
	}
	e.Cell = cell(0, 0, 50, 10)
	if e.Box() != *e.Cell {
		t.Fatal("Box with a cell is the cell")
	}
}

func TestNorm(t *testing.T) {
	cases := map[string]string{
		"Member Number:":         "member number:",
		"  Current \t Balance\n": "current balance",
		"SEARCH":                 "search",
		"":                       "",
	}
	for in, want := range cases {
		if got := Norm(in); got != want {
			t.Errorf("Norm(%q) = %q, want %q", in, got, want)
		}
	}
}
