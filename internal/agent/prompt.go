package agent

import (
	"fmt"
	"strings"

	"github.com/NightWatchEng/rote/internal/capability"
	"github.com/NightWatchEng/rote/internal/runner"
	"github.com/NightWatchEng/rote/internal/surface"
)

// Spec is a capability request: what should exist once discovery succeeds.
// The contract (inputs, outputs) is authored by a person; how to fulfil it
// inside the application is what the model works out.
type Spec struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Goal is the natural-language goal. It refers to inputs by placeholder
	// ({{inputs.member_id}}), so the model plans against the parameter, not
	// against one concrete value.
	Goal    string              `json:"goal"`
	Entry   string              `json:"entry"`
	Inputs  []capability.Param  `json:"inputs"`
	Secrets []string            `json:"secrets"`
	Outputs []capability.Output `json:"outputs"`
}

// Decision is the model's reply for one turn. Every field is always present
// (structured outputs require it); unused ones are empty.
type Decision struct {
	Reason       string `json:"reason"`
	Action       string `json:"action"`
	Ref          int    `json:"ref"`
	Text         string `json:"text"`
	Output       string `json:"output"`
	RowAnchor    string `json:"row_anchor"`
	ColumnAnchor string `json:"column_anchor"`
	Success      bool   `json:"success"`
}

var decisionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"reason":        map[string]any{"type": "string", "description": "One short sentence: why this action serves the goal. Shown to the human reviewer. No data values."},
		"action":        map[string]any{"type": "string", "enum": []string{"click", "type", "select", "extract", "finish", "escalate"}},
		"ref":           map[string]any{"type": "integer", "description": "Ref of the element to act on; 0 when not applicable."},
		"text":          map[string]any{"type": "string", "description": "type: the text (placeholders for inputs/secrets). select: the option label. finish: static on-screen text proving the goal state. Otherwise empty."},
		"output":        map[string]any{"type": "string", "description": "extract: name of the declared output. Otherwise empty."},
		"row_anchor":    map[string]any{"type": "string", "description": "extract from a table: text identifying the row. Otherwise empty."},
		"column_anchor": map[string]any{"type": "string", "description": "extract from a table: the column header text. Otherwise empty."},
		"success":       map[string]any{"type": "boolean", "description": "finish: whether the goal was achieved. Otherwise false."},
	},
	"required":             []string{"reason", "action", "ref", "text", "output", "row_anchor", "column_anchor", "success"},
	"additionalProperties": false,
}

const systemPrompt = `You operate a legacy back-office banking application on behalf of an automation system, one action per turn, to accomplish a goal. What you do is being recorded and will later be replayed without you, for other input values. Each turn you are given the goal, what has been done so far, and the current screen; you reply with exactly one action as JSON.

How the screen is shown
- The application is split into frames. Under each frame header, every line is one visual row read left to right, with items separated by " | ".
- Each item is [ref] role "name". A ref is only valid for the screen it appears on.
- Many form fields have no name of their own: their label is the text on the same row, to their left.
- Some clickable things are plain text rather than buttons or links (menu entries, for example). Click the text.
- Sensitive values are masked ([REDACTED], [AMOUNT], [SSN] and similar). You never need a real value: you point at the element and the system reads it.

Actions
- click: ref of the element.
- type: ref of a text field, and text. This replaces the field's content. Whenever a value comes from an input or a secret, write its placeholder exactly, {{inputs.NAME}} or {{secrets.NAME}}, never a literal value. You are never shown secret values.
- select: ref of a drop-down, and text = the label of the option to choose.
- extract: capture one declared output. Set output to its name and ref to the element that holds the value. If the value is in a table, also set row_anchor (the text that identifies its row, such as an account description) and column_anchor (the column's header text). If the value sits beside a label, leave both anchors empty.
- finish: with success=true once the goal is met and every declared output has been extracted; set text to a short piece of static text visible right now that proves the goal state was reached (a page heading or confirmation message, never data). With success=false if the goal cannot be achieved; say why in reason.
- escalate: hand over to a human operator when you cannot safely proceed.

Rules
- Do only what the goal requires, by the most direct path. Do not explore the application.
- Because the recording is replayed with other inputs, act on labels and structure, never on a specific data value you happen to see.
- If an action is blocked by policy or declined by an operator, do not look for a way around it.
- If the application shows an error or an outcome that makes the goal impossible (for example, the record does not exist), finish with success=false.`

// buildPrompt renders one turn's request. The screen must already be
// redacted.
func buildPrompt(spec *Spec, captured map[string]bool, history []string, screen *surface.Observation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "GOAL\n%s\n", spec.Goal)

	b.WriteString("\nINPUTS (write as {{inputs.NAME}})\n")
	if len(spec.Inputs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, p := range spec.Inputs {
		fmt.Fprintf(&b, "- %s (%s): %s\n", p.Name, p.Type, p.Description)
	}
	b.WriteString("\nSECRETS (write as {{secrets.NAME}})\n")
	if len(spec.Secrets) == 0 {
		b.WriteString("(none)\n")
	}
	for _, s := range spec.Secrets {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	b.WriteString("\nOUTPUTS TO EXTRACT\n")
	if len(spec.Outputs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, o := range spec.Outputs {
		state := "not yet extracted"
		if captured[o.Name] {
			state = "extracted"
		}
		fmt.Fprintf(&b, "- %s (%s): %s [%s]\n", o.Name, o.Type, o.Description, state)
	}

	b.WriteString("\nDONE SO FAR\n")
	if len(history) == 0 {
		b.WriteString("(nothing yet)\n")
	}
	for i, h := range history {
		fmt.Fprintf(&b, "%d. %s\n", i+1, h)
	}

	b.WriteString("\nCURRENT SCREEN\n")
	b.WriteString(strings.Join(runner.ScreenLines(screen, true), "\n"))
	b.WriteString("\n\nReply with the next action.")
	return b.String()
}
