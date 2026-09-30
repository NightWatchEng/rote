// Package redact keeps regulated data out of everything that leaves the live
// session: the text shown to the model, the run log, saved observations and
// screenshots. One Redactor is built per run and applied at each of those
// boundaries, so there is a single place to reason about what can leak.
package redact

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"regexp"
	"sort"
	"strings"

	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/surface"
)

// Mask replaces a value whose content must not be shown at all.
const Mask = "[REDACTED]"

var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), "[SSN]"},
	{regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`), "[CARD]"},
	{regexp.MustCompile(`\(\d{3}\)\s?\d{3}-\d{4}|\b\d{3}[-.]\d{3}[-.]\d{4}\b`), "[PHONE]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[EMAIL]"},
	{regexp.MustCompile(`-?\$\s?\d[\d,]*(?:\.\d{2})?`), "[AMOUNT]"},
}

// Redactor masks by pattern, by known literal (secrets and sensitive inputs
// resolved for this run), and by label (the value next to "SSN/TIN:").
type Redactor struct {
	fieldLabels []string
	literals    []literal
	custom      []rule
}

type rule struct {
	re   *regexp.Regexp
	repl string
}

type literal struct{ value, token string }

// New builds a redactor. fieldLabels are visible labels whose adjacent value
// is sensitive whatever it looks like (names and addresses match no pattern).
func New(fieldLabels []string) *Redactor {
	return &Redactor{fieldLabels: fieldLabels}
}

// AddPattern registers a product-specific rule for sensitive text that no
// generic pattern or label can catch, such as a name embedded in a sentence
// ("Member 10042 — Dana R. Whitfield"). repl may use $1-style groups to keep
// the non-sensitive part.
func (r *Redactor) AddPattern(pattern, repl string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	r.custom = append(r.custom, rule{re, repl})
	return nil
}

// AddLiteral registers a concrete value that must never appear in output.
// Very short values are skipped: replacing every "1" would destroy the log
// without protecting anything.
func (r *Redactor) AddLiteral(kind, name, value string) {
	if len(value) < 4 {
		return
	}
	r.literals = append(r.literals, literal{value, "[" + kind + ":" + name + "]"})
	sort.SliceStable(r.literals, func(i, j int) bool { return len(r.literals[i].value) > len(r.literals[j].value) })
}

// Text masks literals and patterns in a string.
func (r *Redactor) Text(s string) string {
	for _, l := range r.literals {
		s = strings.ReplaceAll(s, l.value, l.token)
	}
	for _, c := range r.custom {
		s = c.re.ReplaceAllString(s, c.repl)
	}
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// Observation returns a redacted copy of an observation, plus the rectangles
// of everything that was masked so a screenshot of the same moment can be
// blacked out in the same places.
func (r *Redactor) Observation(obs *surface.Observation) (*surface.Observation, []surface.Rect) {
	out := *obs
	out.Elements = make([]surface.Element, len(obs.Elements))
	copy(out.Elements, obs.Elements)

	// The value beside a sensitive label is masked whether it is displayed
	// as text or sitting in a form field.
	masked := map[int]bool{}
	for _, f := range obs.Frames {
		for _, label := range r.fieldLabels {
			for _, role := range []string{surface.RoleText, surface.RoleTextbox} {
				t := locate.Target{Frame: f.Name, Locators: []locate.Locator{
					{Strategy: locate.ByLabel, Role: role, Label: label, Side: "left"},
				}}
				if m, err := locate.Resolve(t, obs, true); err == nil {
					masked[m.Element.Ref] = true
				}
			}
		}
	}

	var rects []surface.Rect
	for i := range out.Elements {
		e := &out.Elements[i]
		switch {
		case masked[e.Ref] && e.Role == surface.RoleText:
			e.Name = Mask
			rects = append(rects, e.Bounds)
		case masked[e.Ref]:
			if e.Value != "" {
				e.Value = Mask
			}
			rects = append(rects, e.Bounds)
		case e.Protected:
			if e.Value != "" {
				e.Value = "••••" // filled, but not how long
			}
			rects = append(rects, e.Bounds)
		default:
			name, value := r.Text(e.Name), r.Text(e.Value)
			if name != e.Name || value != e.Value {
				rects = append(rects, e.Bounds)
			}
			e.Name, e.Value = name, value
		}
	}
	return &out, rects
}

// Image blacks out rectangles in a PNG screenshot. Coordinates are surface
// pixels; adapters capture at scale 1 so they map directly.
func Image(shot []byte, rects []surface.Rect) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(shot))
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Src)
	black := image.NewUniform(color.Black)
	for _, rc := range rects {
		box := image.Rect(int(rc.X)-1, int(rc.Y)-1, int(rc.X+rc.W)+2, int(rc.Y+rc.H)+2)
		draw.Draw(dst, box.Intersect(dst.Bounds()), black, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// JSON redacts every string value inside an arbitrary JSON-serializable
// value, preserving field order and numbers. It is the last line of defense
// on the run log: even a field nobody thought of as sensitive passes through
// Text before it is written.
func (r *Redactor) JSON(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := r.rewrite(dec, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (r *Redactor) rewrite(dec *json.Decoder, buf *bytes.Buffer) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		buf.WriteRune(rune(t))
		for first := true; dec.More(); first = false {
			if !first {
				buf.WriteByte(',')
			}
			if t == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				kb, _ := json.Marshal(key)
				buf.Write(kb)
				buf.WriteByte(':')
			}
			if err := r.rewrite(dec, buf); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		buf.WriteRune(rune(end.(json.Delim)))
	case string:
		sb, _ := json.Marshal(r.Text(t))
		buf.Write(sb)
	case json.Number:
		buf.WriteString(t.String())
	default: // bool, nil
		vb, _ := json.Marshal(t)
		buf.Write(vb)
	}
	return nil
}
