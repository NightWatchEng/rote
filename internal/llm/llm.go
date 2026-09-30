// Package llm is the model behind the discovery agent. The agent asks one
// question per turn — "given this goal, this history and this screen, what is
// the next action?" — and gets back one JSON object constrained to a schema.
// Because each call is self-contained, the backends are interchangeable and
// the agent loop above them is identical whichever one is used.
package llm

import (
	"context"
	"encoding/json"
)

// Request is one stateless decision request.
type Request struct {
	System string
	Prompt string
	// Schema is the JSON Schema the reply must conform to.
	Schema map[string]any
}

// Response is the model's structured reply and what it cost.
type Response struct {
	JSON         json.RawMessage
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64 // reported by the backend when it knows; 0 otherwise
}

// Client is a model backend.
type Client interface {
	// Backend names the transport ("anthropic-api", "claude-cli").
	Backend() string
	Model() string
	Complete(ctx context.Context, req Request) (Response, error)
}

// DefaultModel is used when none is configured.
const DefaultModel = "claude-opus-5-5"
