package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// operatorCmd is a command-line operator: the same API the HTML console
// calls, usable from a terminal or a script.
//
//	cua operator wait                 block until an intervention is open, then print it
//	cua operator claim                take control of the live session
//	cua operator click --x N --y N    click in the live session
//	cua operator type --text T        type into the focused field (never logged)
//	cua operator key --key Enter      press Enter | Tab | Backspace | Escape
//	cua operator resolve --action A   approve | deny | resume | step_done | abort
func operatorCmd(args []string) int {
	if len(args) == 0 {
		return fail("operator: want one of wait, claim, click, type, key, resolve")
	}
	verb := args[0]
	fs := flag.NewFlagSet("operator "+verb, flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8090", "operator console address")
	as := fs.String("as", "operator", "operator name, recorded in the run's evidence")
	x := fs.Float64("x", 0, "click: x in live-view pixels")
	y := fs.Float64("y", 0, "click: y in live-view pixels")
	text := fs.String("text", "", "type: text for the focused field")
	key := fs.String("key", "", "key: Enter | Tab | Backspace | Escape")
	action := fs.String("action", "", "resolve: approve | deny | resume | step_done | abort")
	note := fs.String("note", "", "resolve: a note for the record")
	timeout := fs.Duration("timeout", 2*time.Minute, "wait: how long to wait for an intervention")
	fs.Parse(args[1:])
	base := "http://" + *addr

	current := func() (map[string]any, error) {
		resp, err := http.Get(base + "/api/state")
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var st struct {
			Intervention map[string]any `json:"intervention"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			return nil, err
		}
		return st.Intervention, nil
	}
	post := func(path string, body map[string]any) int {
		iv, err := current()
		if err != nil {
			return fail("operator: %v", err)
		}
		if iv == nil {
			return fail("operator: no intervention is open")
		}
		body["id"], body["operator"] = iv["id"], *as
		raw, _ := json.Marshal(body)
		resp, err := http.Post(base+path, "application/json", bytes.NewReader(raw))
		if err != nil {
			return fail("operator: %v", err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "refused: %s", out)
			return exitFailed
		}
		fmt.Printf("%s", out)
		return exitOK
	}

	switch verb {
	case "wait":
		deadline := time.Now().Add(*timeout)
		for time.Now().Before(deadline) {
			if iv, err := current(); err == nil && iv != nil {
				printJSON(iv)
				return exitOK
			}
			time.Sleep(300 * time.Millisecond)
		}
		return fail("operator: no intervention within %s", *timeout)
	case "claim":
		return post("/api/claim", map[string]any{})
	case "click":
		return post("/api/input", map[string]any{"kind": "click", "x": *x, "y": *y})
	case "type":
		return post("/api/input", map[string]any{"kind": "type", "text": *text})
	case "key":
		return post("/api/input", map[string]any{"kind": "key", "key": *key})
	case "resolve":
		return post("/api/resolve", map[string]any{"action": *action, "note": *note})
	}
	return fail("operator: unknown verb %q", verb)
}
