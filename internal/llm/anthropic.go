package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Anthropic calls the Claude Messages API through the official SDK, using
// structured outputs so the reply is guaranteed to match the schema.
type Anthropic struct {
	client anthropic.Client
	model  string
}

// NewAnthropic builds the API backend. Credentials come from the
// environment (ANTHROPIC_API_KEY); opts exist for tests.
func NewAnthropic(model string, opts ...option.RequestOption) *Anthropic {
	if model == "" {
		model = DefaultModel
	}
	return &Anthropic{client: anthropic.NewClient(opts...), model: model}
}

func (a *Anthropic) Backend() string { return "anthropic-api" }
func (a *Anthropic) Model() string   { return a.model }

func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	msg, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 16000,
		System:    []anthropic.TextBlockParam{{Text: req.System}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			// One UI decision per call is routine work; medium effort is
			// plenty and keeps each turn quick.
			Effort: anthropic.OutputConfigEffortMedium,
			Format: anthropic.JSONOutputFormatParam{Schema: req.Schema},
		},
	})
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case 401, 403:
				return Response{}, fmt.Errorf("the API rejected the credentials (HTTP %d); check ANTHROPIC_API_KEY: %w", apiErr.StatusCode, err)
			case 429:
				return Response{}, fmt.Errorf("rate limited by the API after retries: %w", err)
			}
			return Response{}, fmt.Errorf("API error (HTTP %d): %w", apiErr.StatusCode, err)
		}
		return Response{}, fmt.Errorf("could not reach the API: %w", err)
	}
	switch msg.StopReason {
	case anthropic.StopReasonRefusal:
		return Response{}, fmt.Errorf("the model declined the request (%s)", msg.StopDetails.Category)
	case anthropic.StopReasonMaxTokens:
		return Response{}, errors.New("the reply was cut off at max_tokens")
	}
	var text strings.Builder
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	if text.Len() == 0 {
		return Response{}, errors.New("the reply contained no text")
	}
	return Response{JSON: []byte(text.String()), InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens}, nil
}
