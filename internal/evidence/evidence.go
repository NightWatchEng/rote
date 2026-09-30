// Package evidence writes what a run did and why: an append-only JSONL event
// log plus richer files (screenshots, observation snapshots, the result).
// Everything written here passes through the run's Redactor first.
package evidence

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NightWatchEng/rote/internal/redact"
	"github.com/NightWatchEng/rote/internal/surface"
)

// Event is one line of events.jsonl.
type Event struct {
	TS  string `json:"ts"`
	Run string `json:"run"`
	Seq int    `json:"seq"`
	// Actor is who caused the event: "automation", "human:<operator>",
	// "policy" or "system". It makes control transfers legible in the log.
	Actor string          `json:"actor"`
	Type  string          `json:"event"`
	Step  string          `json:"step,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Run is the evidence sink for one discovery or replay run.
type Run struct {
	ID  string
	Dir string

	red  *redact.Redactor
	echo io.Writer

	mu  sync.Mutex
	f   *os.File
	seq int
}

// Start creates the run directory and opens the event log. If dir is empty
// the run is placed under runs/<id>.
func Start(dir, kind string, red *redact.Redactor, echo io.Writer) (*Run, error) {
	buf := make([]byte, 3)
	rand.Read(buf)
	id := fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102T150405"), kind, hex.EncodeToString(buf))
	if dir == "" {
		dir = filepath.Join("runs", id)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	return &Run{ID: id, Dir: dir, red: red, echo: echo, f: f}, nil
}

// Log appends an event. data may be nil or any JSON-serializable value.
func (r *Run) Log(actor, typ, step string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	ev := Event{TS: time.Now().UTC().Format(time.RFC3339Nano), Run: r.ID, Seq: r.seq, Actor: actor, Type: typ, Step: step}
	if data != nil {
		raw, err := r.red.JSON(data)
		if err != nil {
			raw, _ = json.Marshal(map[string]string{"log_error": err.Error()})
		}
		ev.Data = raw
	}
	line, _ := json.Marshal(ev)
	r.f.Write(append(line, '\n'))
	if r.echo != nil {
		step := ev.Step
		if step != "" {
			step = " " + step
		}
		fmt.Fprintf(r.echo, "%s  %-16s %-22s%s %s\n", time.Now().Format("15:04:05"), ev.Actor, ev.Type, step, string(ev.Data))
	}
}

// Screenshot stores a PNG with the given rectangles blacked out and returns
// the file name. A screenshot that cannot be redacted is not stored.
func (r *Run) Screenshot(name string, shot []byte, masked []surface.Rect) string {
	safe, err := redact.Image(shot, masked)
	if err != nil {
		r.Log("system", "evidence.screenshot_dropped", "", map[string]string{"name": name, "reason": err.Error()})
		return ""
	}
	if err := os.WriteFile(filepath.Join(r.Dir, name), safe, 0o644); err != nil {
		return ""
	}
	return name
}

// SaveJSON stores a redacted JSON document alongside the log.
func (r *Run) SaveJSON(name string, v any) string {
	raw, err := r.red.JSON(v)
	if err != nil {
		return ""
	}
	var out bytes.Buffer
	json.Indent(&out, raw, "", "  ")
	out.WriteByte('\n')
	if err := os.WriteFile(filepath.Join(r.Dir, name), out.Bytes(), 0o644); err != nil {
		return ""
	}
	return name
}

func (r *Run) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
