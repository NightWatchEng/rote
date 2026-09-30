package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

var testSchema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"action": map[string]any{"type": "string"}},
	"required":             []any{"action"},
	"additionalProperties": false,
}

var testRequest = Request{System: "You drive a teller workstation.", Prompt: "Goal: open a sub-account. Next action?", Schema: testSchema}

const okMessage = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"{\"action\":\"click\"}"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":5}}`

// fakeAPI serves one canned reply to /v1/messages and keeps the decoded
// request body for inspection.
func fakeAPI(t *testing.T, status int, reply string) (*Anthropic, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("request body is not JSON: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	a := NewAnthropic("claude-opus-5-5", option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	return a, &got
}

func TestAnthropicRequestAndReply(t *testing.T) {
	a, got := fakeAPI(t, http.StatusOK, okMessage)
	resp, err := a.Complete(context.Background(), testRequest)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"action":"click"}` || resp.InputTokens != 12 || resp.OutputTokens != 5 {
		t.Fatalf("response = %s in=%d out=%d", resp.JSON, resp.InputTokens, resp.OutputTokens)
	}

	req := *got
	if req["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v", req["model"])
	}
	system, _ := req["system"].([]any)
	if len(system) != 1 || system[0].(map[string]any)["text"] != testRequest.System {
		t.Errorf("system = %v", req["system"])
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", req["messages"])
	}
	msg := msgs[0].(map[string]any)
	content := msg["content"].([]any)[0].(map[string]any)
	if msg["role"] != "user" || content["text"] != testRequest.Prompt {
		t.Errorf("user message = %v", msg)
	}

	oc, _ := req["output_config"].(map[string]any)
	if oc["effort"] != "medium" {
		t.Errorf("output_config.effort = %v, want medium", oc["effort"])
	}
	wantFormat := map[string]any{"type": "json_schema", "schema": roundTrip(t, testSchema)}
	if !reflect.DeepEqual(oc["format"], wantFormat) {
		t.Errorf("output_config.format = %v, want %v", oc["format"], wantFormat)
	}
	// Structured outputs replace forced tool use entirely.
	for _, k := range []string{"tool_choice", "tools"} {
		if _, ok := req[k]; ok {
			t.Errorf("request carries %s: %v", k, req[k])
		}
	}
}

func roundTrip(t *testing.T, v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	json.Unmarshal(raw, &out)
	return out
}

func TestAnthropicFailures(t *testing.T) {
	message := func(stop, content string) string {
		return `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":` + content +
			`,"stop_reason":"` + stop + `","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":null},"usage":{"input_tokens":12,"output_tokens":5}}`
	}
	text := `[{"type":"text","text":"{\"act"}]`
	cases := []struct {
		name   string
		status int
		reply  string
		want   string
	}{
		{"refusal", 200, message("refusal", text), "declined"},
		{"max tokens", 200, message("max_tokens", text), "max_tokens"},
		{"thinking only", 200, message("end_turn", `[{"type":"thinking","thinking":"hmm","signature":"sig"}]`), "no text"},
		{"no content", 200, message("end_turn", `[]`), "no text"},
		{"bad key", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "credentials"},
		{"server error", 500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := fakeAPI(t, tc.status, tc.reply)
			resp, err := a.Complete(context.Background(), testRequest)
			if err == nil {
				t.Fatalf("no error; response %s", resp.JSON)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestAnthropicDefaultsModel(t *testing.T) {
	if m := NewAnthropic("").Model(); m != DefaultModel {
		t.Fatalf("model = %s, want %s", m, DefaultModel)
	}
}

// fakeClaude installs a `claude` executable on PATH that records its argv and
// stdin, prints reply, and exits with code.
func fakeClaude(t *testing.T, reply string, code int) (argvFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile, stdinFile = filepath.Join(dir, "argv"), filepath.Join(dir, "stdin")
	replyFile := filepath.Join(dir, "reply")
	if err := os.WriteFile(replyFile, []byte(reply), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		`for a in "$@"; do printf '%s\0' "$a"; done > '` + argvFile + "'\n" +
		`cat > '` + stdinFile + "'\n" +
		`cat '` + replyFile + "'\n" +
		`echo 'fake claude stderr' >&2` + "\n" +
		"exit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile, stdinFile
}

const cliOK = `{"is_error":false,"result":"done","structured_output":{"action":"click"},"total_cost_usd":0.01,"usage":{"input_tokens":2,"cache_creation_input_tokens":10,"cache_read_input_tokens":3,"output_tokens":7}}`

func TestClaudeCLIHappyPath(t *testing.T) {
	argvFile, stdinFile := fakeClaude(t, cliOK, 0)
	c, err := NewClaudeCLI("claude-test-model")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Complete(context.Background(), testRequest)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"action":"click"}` || resp.InputTokens != 15 || resp.OutputTokens != 7 || resp.CostUSD != 0.01 {
		t.Fatalf("response = %s in=%d out=%d cost=%v", resp.JSON, resp.InputTokens, resp.OutputTokens, resp.CostUSD)
	}

	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	schema, _ := json.Marshal(testSchema)
	for _, flag := range []string{"-p", "--safe-mode", "--no-session-persistence"} {
		if indexOf(args, flag) < 0 {
			t.Errorf("argv lacks %s: %q", flag, args)
		}
	}
	for flag, want := range map[string]string{
		"--tools":         "",
		"--output-format": "json",
		"--json-schema":   string(schema),
		"--system-prompt": testRequest.System,
		"--model":         "claude-test-model",
	} {
		i := indexOf(args, flag)
		if i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Errorf("argv %s = %q, want %q (argv %q)", flag, valueAfter(args, i), want, args)
		}
	}
	// The prompt goes on stdin, never on the command line.
	if stdin, _ := os.ReadFile(stdinFile); string(stdin) != testRequest.Prompt {
		t.Errorf("stdin = %q, want the prompt", stdin)
	}
	if indexOf(args, testRequest.Prompt) >= 0 {
		t.Error("prompt was passed as an argument")
	}
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

func valueAfter(xs []string, i int) string {
	if i < 0 || i+1 >= len(xs) {
		return "<missing>"
	}
	return xs[i+1]
}

func TestClaudeCLIFailures(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		code  int
		want  string
	}{
		{"reported error", `{"is_error":true,"result":"Not logged in"}`, 0, "Not logged in"},
		{"no structured output", `{"is_error":false,"result":"I think you should click"}`, 0, "no structured output"},
		{"null structured output", `{"is_error":false,"result":"x","structured_output":null}`, 0, "no structured output"},
		{"unparseable output", `not json`, 0, "unparseable"},
		{"non-zero exit", cliOK, 3, "fake claude stderr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClaude(t, tc.reply, tc.code)
			c, err := NewClaudeCLI("")
			if err != nil {
				t.Fatal(err)
			}
			if c.Model() != DefaultModel {
				t.Errorf("model = %s, want default", c.Model())
			}
			_, err = c.Complete(context.Background(), testRequest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestNewClaudeCLIRequiresBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := NewClaudeCLI(""); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("err = %v, want not-on-PATH error", err)
	}
}
