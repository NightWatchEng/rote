package redact

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"testing"

	"github.com/NightWatchEng/rote/internal/surface"
)

func TestTextPatterns(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"ssn", "SSN 123-45-6789 on file", "SSN [SSN] on file"},
		{"card spaced", "card 4111 1111 1111 1111", "card [CARD]"},
		{"card dashed", "card 4111-1111-1111-1111.", "card [CARD]."},
		{"card run", "pan=4111111111111111", "pan=[CARD]"},
		{"phone", "call (503) 555-0142 today", "call [PHONE] today"},
		{"phone unspaced", "(503)555-0142", "[PHONE]"},
		{"email", "mail j.doe+stmt@bank.example.org now", "mail [EMAIL] now"},
		{"amount", "Current Balance $5,230.18", "Current Balance [AMOUNT]"},
		{"negative amount", "fee -$12.00 posted", "fee [AMOUNT] posted"},
		{"whole dollars", "deposit $ 40", "deposit [AMOUNT]"},
		{"several at once", "123-45-6789 / (503) 555-0142 / $1.00", "[SSN] / [PHONE] / [AMOUNT]"},

		{"member number is not a card", "Member 10042", "Member 10042"},
		{"date is not an ssn", "opened 2026-09-30", "opened 2026-09-30"},
		{"plain words", "Regular Savings", "Regular Savings"},
	}
	r := New(nil)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.Text(c.in); got != c.want {
				t.Fatalf("Text(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestTextLiterals(t *testing.T) {
	r := New(nil)
	r.AddLiteral("secret", "pin", "742") // too short to mask usefully
	r.AddLiteral("input", "first", "Dana")
	r.AddLiteral("input", "full", "Dana Whitfield")
	r.AddLiteral("secret", "operator_password", "s3cr3t-Pass")
	r.AddLiteral("input", "ssn", "000-45-6789")

	cases := []struct{ name, in, want string }{
		{"secret", "typed s3cr3t-Pass into Password", "typed [secret:operator_password] into Password"},
		{"short literal skipped", "code 742", "code 742"},
		// Registered short-first, still replaced longest-first so the full
		// name is not split into "[input:first] Whitfield".
		{"longest first", "Dana Whitfield called", "[input:full] called"},
		{"shorter still masked alone", "Dana called", "[input:first] called"},
		{"literal wins over pattern", "SSN 000-45-6789", "SSN [input:ssn]"},
		{"every occurrence", "Dana, Dana", "[input:first], [input:first]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.Text(c.in); got != c.want {
				t.Fatalf("Text(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func text(ref int, name string, x, y, w float64) surface.Element {
	return surface.Element{Ref: ref, Frame: "main", Role: surface.RoleText, Name: name, Bounds: surface.Rect{X: x, Y: y, W: w, H: 20}}
}

func textbox(ref int, value string, protected bool, x, y float64) surface.Element {
	return surface.Element{Ref: ref, Frame: "main", Role: surface.RoleTextbox, Value: value, Protected: protected,
		Bounds: surface.Rect{X: x, Y: y, W: 160, H: 22}}
}

// memberDetail is the member screen with labelled personal data, a password
// box and free text that only patterns can catch.
func memberDetail() *surface.Observation {
	return &surface.Observation{
		Frames: []surface.Frame{{Name: "main", URL: "http://127.0.0.1:8080/member?no=10042", Title: "Member Detail"}},
		Elements: []surface.Element{
			text(1, "Member Name:", 20, 100, 100),
			text(2, "Dana R. Whitfield", 130, 100, 130),
			text(3, "SSN/TIN:", 20, 130, 70),
			text(4, "000-45-6789", 130, 130, 90),
			text(5, "Status:", 20, 160, 60),
			text(6, "Active", 130, 160, 50),
			textbox(7, "hunter22", true, 130, 190),
			textbox(8, "", true, 130, 220),
			text(9, "Contact dana@example.com", 20, 250, 200),
			text(10, "Balance $5,230.18", 20, 280, 150),
			textbox(11, "000-61-2204", false, 130, 310),
		},
	}
}

func TestObservation(t *testing.T) {
	in := memberDetail()
	r := New([]string{"Member Name:", "SSN/TIN:", "Phone:"})
	out, rects := r.Observation(in)

	want := map[int]struct{ name, value string }{
		2:  {Mask, ""},               // label-adjacent: no pattern would catch a name
		4:  {Mask, ""},               // label-adjacent, masked outright rather than as [SSN]
		6:  {"Active", ""},           // label not listed
		7:  {"", "••••"},             // protected: filled, length hidden
		8:  {"", ""},                 // protected but empty stays empty
		9:  {"Contact [EMAIL]", ""},  // pattern in name
		10: {"Balance [AMOUNT]", ""}, // pattern in name
		11: {"", "[SSN]"},            // pattern in value
		1:  {"Member Name:", ""},     // labels themselves are not data
	}
	for ref, w := range want {
		e, _ := out.ByRef(ref)
		if e.Name != w.name || e.Value != w.value {
			t.Errorf("ref %d: name %q value %q, want %q %q", ref, e.Name, e.Value, w.name, w.value)
		}
	}

	wantRects := map[surface.Rect]bool{}
	for _, ref := range []int{2, 4, 7, 8, 9, 10, 11} {
		e, _ := in.ByRef(ref)
		wantRects[e.Bounds] = true
	}
	if len(rects) != len(wantRects) {
		t.Fatalf("got %d rects, want %d: %v", len(rects), len(wantRects), rects)
	}
	for _, rc := range rects {
		if !wantRects[rc] {
			t.Errorf("unexpected masked rect %v", rc)
		}
	}

	if !reflect.DeepEqual(in, memberDetail()) {
		t.Fatal("the input observation was modified")
	}
}

func TestObservationLabelScopedToFrame(t *testing.T) {
	// A listed label in one frame does not mask a value that merely lines up
	// with it in another frame.
	obs := memberDetail()
	obs.Frames = append(obs.Frames, surface.Frame{Name: "nav"})
	obs.Elements = append(obs.Elements, surface.Element{Ref: 20, Frame: "nav", Role: surface.RoleText, Name: "Address:",
		Bounds: surface.Rect{X: 20, Y: 160, W: 70, H: 20}})
	out, _ := New([]string{"Address:"}).Observation(obs)
	if e, _ := out.ByRef(6); e.Name != "Active" {
		t.Fatalf("value in another frame was masked: %q", e.Name)
	}
}

// A field label masks the value adjacent to it "whatever it looks like". On
// an entry form that value lives in a textbox, not in text.
func TestObservationMasksLabelledTextbox(t *testing.T) {
	obs := &surface.Observation{
		Frames: []surface.Frame{{Name: "main"}},
		Elements: []surface.Element{
			text(1, "Nickname:", 20, 100, 80),
			textbox(2, "Rainy Day Fund", false, 130, 99),
		},
	}
	out, rects := New([]string{"Nickname:"}).Observation(obs)
	if e, _ := out.ByRef(2); e.Value == "Rainy Day Fund" || len(rects) == 0 {
		t.Fatalf("a textbox value beside a sensitive label was left in clear: value %q, rects %v", e.Value, rects)
	}
}

func TestJSON(t *testing.T) {
	type entry struct {
		Zeta   string      `json:"zeta"`
		Alpha  uint64      `json:"alpha"`
		Amount json.Number `json:"amount"`
		Ratio  float64     `json:"ratio"`
		Items  []any       `json:"items"`
		Empty  struct{}    `json:"empty"`
		Flag   bool        `json:"flag"`
	}
	v := entry{
		Zeta:   "ssn 123-45-6789",
		Alpha:  18446744073709551615,
		Amount: "5230.10",
		Ratio:  1e21,
		Items:  []any{"mail a@b.co", 3, map[string]any{"b": "$1.00", "a": true}, nil, []any{}},
		Flag:   true,
	}
	got, err := New(nil).JSON(v)
	if err != nil {
		t.Fatal(err)
	}
	// Field order stays as declared (not sorted), and numbers keep their
	// exact text: no float round trip for the uint64 or the trailing zero.
	want := `{"zeta":"ssn [SSN]","alpha":18446744073709551615,"amount":5230.10,"ratio":1e+21,` +
		`"items":["mail [EMAIL]",3,{"a":true,"b":"[AMOUNT]"},null,[]],"empty":{},"flag":true}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if !json.Valid(got) {
		t.Fatal("output is not valid JSON")
	}
}

func TestJSONScalarsAndErrors(t *testing.T) {
	r := New(nil)
	r.AddLiteral("secret", "pw", "s3cr3t-Pass")
	got, err := r.JSON("login with s3cr3t-Pass")
	if err != nil || string(got) != `"login with [secret:pw]"` {
		t.Fatalf("got %s (%v)", got, err)
	}
	// Keys are structure, not data, and are left alone.
	got, _ = r.JSON(map[string]string{"s3cr3t-Pass": "x"})
	if string(got) != `{"s3cr3t-Pass":"x"}` {
		t.Fatalf("got %s", got)
	}
	if _, err := r.JSON(make(chan int)); err == nil {
		t.Fatal("an unmarshalable value is an error")
	}
}

func testPNG(t *testing.T, w, h int, fill color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestImage(t *testing.T) {
	red := color.RGBA{R: 255, A: 255}
	shot := testPNG(t, 40, 40, red)
	out, err := Image(shot, []surface.Rect{
		{X: 10, Y: 10, W: 6, H: 4},
		{X: 35, Y: 35, W: 20, H: 20}, // runs off the edge: clipped, not a panic
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, 40, 40) {
		t.Fatalf("size changed: %v", img.Bounds())
	}
	isBlack := func(x, y int) bool {
		r, g, b, _ := img.At(x, y).RGBA()
		return r == 0 && g == 0 && b == 0
	}
	for _, p := range [][2]int{{10, 10}, {15, 13}, {12, 12}, {39, 39}, {36, 38}} {
		if !isBlack(p[0], p[1]) {
			t.Errorf("pixel %v inside a rect is not blacked out", p)
		}
	}
	for _, p := range [][2]int{{0, 0}, {5, 5}, {25, 12}, {12, 25}, {30, 30}, {39, 0}} {
		if r, g, b, a := img.At(p[0], p[1]).RGBA(); r>>8 != 255 || g != 0 || b != 0 || a>>8 != 255 {
			t.Errorf("pixel %v outside every rect changed", p)
		}
	}

	if _, err := Image([]byte("not a png"), nil); err == nil {
		t.Fatal("a non-PNG screenshot is an error")
	}
}

func TestAddPattern(t *testing.T) {
	r := New(nil)
	if err := r.AddPattern(`^(Member \d+) — .+$`, "$1 — [REDACTED]"); err != nil {
		t.Fatal(err)
	}
	if got := r.Text("Member 10042 — Dana R. Whitfield"); got != "Member 10042 — [REDACTED]" {
		t.Errorf("got %q", got)
	}
	if got := r.Text("Member Detail"); got != "Member Detail" {
		t.Errorf("unrelated text changed to %q", got)
	}
	if err := r.AddPattern("(", ""); err == nil {
		t.Error("an invalid pattern was accepted")
	}
}
