package capability

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Render fills {{inputs.x}} and {{secrets.y}} references in a template.
func Render(tpl string, inputs, secrets map[string]string) (string, error) {
	var missing string
	out := refPattern.ReplaceAllStringFunc(tpl, func(ref string) string {
		m := refPattern.FindStringSubmatch(ref)
		src := inputs
		if m[1] == "secrets" {
			src = secrets
		}
		v, ok := src[m[2]]
		if !ok {
			missing = m[1] + "." + m[2]
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("no value for %s", missing)
	}
	return out, nil
}

// UsesSecret reports whether a template references any secret. Steps that do
// are never read back or logged with their value.
func UsesSecret(tpl string) bool {
	for _, m := range refPattern.FindAllStringSubmatch(tpl, -1) {
		if m[1] == "secrets" {
			return true
		}
	}
	return false
}

// CheckInputs validates caller-supplied inputs against the contract before
// the application is touched at all.
func (a *Artifact) CheckInputs(in map[string]string) error {
	return CheckParams(a.Inputs, in)
}

// CheckParams validates values against parameter declarations.
func CheckParams(params []Param, in map[string]string) error {
	declared := map[string]bool{}
	for _, p := range params {
		declared[p.Name] = true
		v, ok := in[p.Name]
		if !ok {
			return fmt.Errorf("missing input %q (%s)", p.Name, p.Description)
		}
		switch p.Type {
		case "integer":
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return fmt.Errorf("input %q must be an integer", p.Name)
			}
		case "money":
			if _, err := ParseValue("money", v); err != nil {
				return fmt.Errorf("input %q must be a dollar amount", p.Name)
			}
		case "enum":
			if !slices.Contains(p.Enum, v) {
				return fmt.Errorf("input %q must be one of: %s", p.Name, strings.Join(p.Enum, ", "))
			}
		}
		if p.Pattern != "" {
			if ok, _ := regexp.MatchString(p.Pattern, v); !ok {
				return fmt.Errorf("input %q does not match the required pattern %s", p.Name, p.Pattern)
			}
		}
	}
	for name := range in {
		if !declared[name] {
			return fmt.Errorf("unknown input %q", name)
		}
	}
	return nil
}

var moneyPattern = regexp.MustCompile(`^(-)?\$?\s*(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d{2}))?$`)

// ParseValue converts text read from the screen into a typed output. A value
// that does not parse as its declared type is a checkpoint failure: the
// screen did not show what the capability promises to return.
func ParseValue(typ, raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	switch typ {
	case "string":
		if raw == "" {
			return nil, fmt.Errorf("empty value")
		}
		return raw, nil
	case "integer":
		n, err := strconv.ParseInt(strings.ReplaceAll(raw, ",", ""), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", raw)
		}
		return n, nil
	case "money":
		// Money is returned as a decimal string ("5230.18"), never a float.
		m := moneyPattern.FindStringSubmatch(raw)
		if m == nil {
			return nil, fmt.Errorf("%q is not a dollar amount", raw)
		}
		cents := m[3]
		if cents == "" {
			cents = "00"
		}
		return m[1] + strings.ReplaceAll(m[2], ",", "") + "." + cents, nil
	}
	return nil, fmt.Errorf("unknown output type %q", typ)
}

// Tool is the agent-facing description of a capability, in the shape of a
// function-calling tool definition: what it does, what it needs, what comes
// back, and which non-success outcomes to expect.
type Tool struct {
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
	Returns     []Output       `json:"returns"`
	Outcomes    []Outcome      `json:"outcomes"`
	Risk        string         `json:"risk"`
	Approved    bool           `json:"approved"`
}

// Tool derives the caller-facing contract from the artifact.
func (a *Artifact) Tool() Tool {
	props := map[string]any{}
	required := []string{}
	for _, p := range a.Inputs {
		prop := map[string]any{"description": p.Description}
		switch p.Type {
		case "integer":
			prop["type"] = "integer"
		default:
			prop["type"] = "string"
		}
		if p.Pattern != "" {
			prop["pattern"] = p.Pattern
		}
		if len(p.Enum) > 0 {
			prop["enum"] = p.Enum
		}
		props[p.Name] = prop
		required = append(required, p.Name)
	}
	return Tool{
		Name:        a.ID,
		Version:     a.Version,
		Description: a.Description,
		InputSchema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false},
		Returns:     a.Outputs,
		Outcomes:    a.Outcomes,
		Risk:        string(a.Risk),
		Approved:    a.Approved(),
	}
}

// LiteralPart checks that every reference in a template is declared and
// returns what is left when the references are removed: the literal text
// that would be stored in the artifact as-is.
func LiteralPart(tpl string, inputs []Param, secrets []string) (string, error) {
	for _, m := range refPattern.FindAllStringSubmatch(tpl, -1) {
		switch m[1] {
		case "inputs":
			if !slices.ContainsFunc(inputs, func(p Param) bool { return p.Name == m[2] }) {
				return "", fmt.Errorf("{{inputs.%s}} is not a declared input", m[2])
			}
		case "secrets":
			if !slices.Contains(secrets, m[2]) {
				return "", fmt.Errorf("{{secrets.%s}} is not a declared secret", m[2])
			}
		}
	}
	literal := refPattern.ReplaceAllString(tpl, "")
	if strings.Contains(literal, "{{") || strings.Contains(literal, "}}") {
		return "", fmt.Errorf("malformed placeholder; use {{inputs.NAME}} or {{secrets.NAME}}")
	}
	return literal, nil
}

// UsesSensitive reports whether a template references a secret or an input
// declared sensitive. Such values are never quoted in a failure report.
func UsesSensitive(tpl string, inputs []Param) bool {
	for _, m := range refPattern.FindAllStringSubmatch(tpl, -1) {
		if m[1] == "secrets" {
			return true
		}
		if slices.ContainsFunc(inputs, func(p Param) bool { return p.Name == m[2] && p.Sensitive }) {
			return true
		}
	}
	return false
}

// MaskSensitive returns a copy of the inputs that is safe to log: values of
// sensitive parameters are replaced whatever their length.
func MaskSensitive(params []Param, in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	for _, p := range params {
		if _, ok := out[p.Name]; ok && p.Sensitive {
			out[p.Name] = "[input:" + p.Name + "]"
		}
	}
	return out
}
