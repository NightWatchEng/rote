package locate

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/NightWatchEng/rote/internal/surface"
)

// Screen names where in the application we are: a frame showing a given
// path and/or title. For a desktop adapter the same fields carry the window
// name and its title.
type Screen struct {
	Frame string `json:"frame"`
	Path  string `json:"path,omitempty"`
	Title string `json:"title,omitempty"`
}

func (s Screen) String() string {
	parts := []string{"frame " + s.Frame}
	if s.Path != "" {
		parts = append(parts, "at "+s.Path)
	}
	if s.Title != "" {
		parts = append(parts, fmt.Sprintf("titled %q", s.Title))
	}
	return strings.Join(parts, " ")
}

// Condition is the one small predicate language used for step checkpoints,
// the capability's success condition and the app profile's state signatures.
// Exactly one of Text, Screen, All, Any is set.
type Condition struct {
	// Text holds when some element (in Frame, if given) contains it.
	Text   string      `json:"text,omitempty"`
	Frame  string      `json:"frame,omitempty"`
	Screen *Screen     `json:"screen,omitempty"`
	All    []Condition `json:"all,omitempty"`
	Any    []Condition `json:"any,omitempty"`
}

func (c Condition) String() string {
	switch {
	case c.Text != "":
		if c.Frame != "" {
			return fmt.Sprintf("text %q visible in frame %s", c.Text, c.Frame)
		}
		return fmt.Sprintf("text %q visible", c.Text)
	case c.Screen != nil:
		return c.Screen.String()
	case len(c.All) > 0:
		return "all of (" + joinConds(c.All) + ")"
	case len(c.Any) > 0:
		return "any of (" + joinConds(c.Any) + ")"
	}
	return "always"
}

func joinConds(cs []Condition) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.String()
	}
	return strings.Join(parts, "; ")
}

// Eval reports whether the condition holds and, for text conditions, the full
// text that satisfied it (used as the human-readable detail of an outcome).
func (c Condition) Eval(obs *surface.Observation) (bool, string) {
	switch {
	case c.Text != "":
		// Of everything that matches, report the longest: a message line
		// says more than the heading that happens to share its words.
		want, best, found := Norm(c.Text), "", false
		for _, e := range obs.Elements {
			if c.Frame != "" && e.Frame != c.Frame {
				continue
			}
			if strings.Contains(Norm(e.Name), want) {
				found = true
				if len(e.Name) > len(best) {
					best = e.Name
				}
			}
		}
		return found, best
	case c.Screen != nil:
		return OnScreen(obs, *c.Screen), ""
	case len(c.All) > 0:
		var detail string
		for _, sub := range c.All {
			ok, d := sub.Eval(obs)
			if !ok {
				return false, ""
			}
			if detail == "" {
				detail = d
			}
		}
		return true, detail
	case len(c.Any) > 0:
		for _, sub := range c.Any {
			if ok, d := sub.Eval(obs); ok {
				return true, d
			}
		}
		return false, ""
	}
	return true, ""
}

// OnScreen reports whether a frame currently shows the given screen.
func OnScreen(obs *surface.Observation, s Screen) bool {
	f, ok := obs.FrameByName(s.Frame)
	if !ok {
		return false
	}
	if s.Path != "" && PathOf(f.URL) != s.Path {
		return false
	}
	if s.Title != "" && Norm(f.Title) != Norm(s.Title) {
		return false
	}
	return true
}

// PathOf reduces a frame location to the part that identifies a screen: the
// path, without host (tenant-specific) or query (record-specific).
func PathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Path
}
