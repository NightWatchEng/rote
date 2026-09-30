package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ClaudeCLI gets its decisions from the `claude` command-line tool in
// non-interactive mode, which authenticates with the user's existing Claude
// login instead of an API key. Tools are disabled and customizations are
// off: it is used purely as a model endpoint with a JSON schema.
type ClaudeCLI struct {
	model string
	bin   string
}

func NewClaudeCLI(model string) (*ClaudeCLI, error) {
	if model == "" {
		model = DefaultModel
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		return nil, errors.New("the claude CLI is not on PATH; install Claude Code or use --backend anthropic with ANTHROPIC_API_KEY")
	}
	return &ClaudeCLI{model: model, bin: bin}, nil
}

func (c *ClaudeCLI) Backend() string { return "claude-cli" }
func (c *ClaudeCLI) Model() string   { return c.model }

func (c *ClaudeCLI) Complete(ctx context.Context, req Request) (Response, error) {
	schema, err := json.Marshal(req.Schema)
	if err != nil {
		return Response{}, err
	}
	cmd := exec.CommandContext(ctx, c.bin, "-p",
		"--safe-mode", "--tools", "", "--no-session-persistence",
		"--model", c.model,
		"--output-format", "json",
		"--system-prompt", req.System,
		"--json-schema", string(schema),
	)
	cmd.Dir = os.TempDir() // no project context leaks into the decision
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Response{}, fmt.Errorf("claude CLI failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var reply struct {
		IsError          bool            `json:"is_error"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		TotalCostUSD     float64         `json:"total_cost_usd"`
		Usage            struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return Response{}, fmt.Errorf("claude CLI returned unparseable output: %w", err)
	}
	if reply.IsError {
		return Response{}, fmt.Errorf("claude CLI reported an error: %s", reply.Result)
	}
	if len(reply.StructuredOutput) == 0 || string(reply.StructuredOutput) == "null" {
		return Response{}, fmt.Errorf("claude CLI returned no structured output: %s", reply.Result)
	}
	return Response{
		JSON:         reply.StructuredOutput,
		InputTokens:  reply.Usage.InputTokens + reply.Usage.CacheCreationTokens + reply.Usage.CacheReadTokens,
		OutputTokens: reply.Usage.OutputTokens,
		CostUSD:      reply.TotalCostUSD,
	}, nil
}
