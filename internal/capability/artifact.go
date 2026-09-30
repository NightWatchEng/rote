// Package capability defines the artifact: the typed, versioned, reviewable
// description of one thing an AI agent can ask this system to do inside a
// legacy application. It also defines the two things an artifact is combined
// with at run time — the app profile (what is true of the vendor product for
// every tenant) and the tenant binding (what is true of one institution).
package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"

	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
)

// SchemaVersion is the version of the artifact format itself. It changes
// when the meaning of a field changes; the replay engine refuses versions it
// does not know rather than guessing.
const SchemaVersion = "1.0"

// Step actions.
const (
	ActionClick   = "click"
	ActionType    = "type"
	ActionSelect  = "select"
	ActionExtract = "extract"
)

// Approval states.
const (
	StatusDraft    = "draft"
	StatusApproved = "approved"
)

// Artifact is a capability: a contract (inputs, outputs, outcomes) plus the
// recorded flow that fulfils it.
type Artifact struct {
	SchemaVersion string `json:"schema_version"`
	// ID names the capability for callers, e.g. "member.get_savings_balance".
	ID string `json:"id"`
	// Version is the semantic version of this capability's contract and flow.
	Version     string `json:"version"`
	Title       string `json:"title"`
	Description string `json:"description"`

	App AppRef `json:"app"`

	// The contract.
	Inputs   []Param   `json:"inputs"`
	Secrets  []string  `json:"secrets"`
	Outputs  []Output  `json:"outputs"`
	Outcomes []Outcome `json:"outcomes"`

	// The flow.
	Entry   string           `json:"entry"`
	Steps   []Step           `json:"steps"`
	Success locate.Condition `json:"success"`

	// Risk is the highest risk of any step, so a reviewer or caller can tell
	// at a glance whether this capability writes to the system of record.
	Risk policy.Risk `json:"risk"`

	Approval   Approval   `json:"approval"`
	Provenance Provenance `json:"provenance"`
}

// AppRef ties the artifact to a vendor product, not to a tenant.
type AppRef struct {
	Profile        string `json:"profile"`
	ProfileVersion string `json:"profile_version"`
	Surface        string `json:"surface"`
}

// Param is a typed input the caller supplies per invocation.
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"` // string | integer | money | enum
	Description string   `json:"description"`
	Pattern     string   `json:"pattern,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	// Sensitive inputs are masked wherever the run is logged.
	Sensitive bool `json:"sensitive,omitempty"`
}

// Output is a typed value the capability returns.
type Output struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string | integer | money
	Description string `json:"description"`
	// Sensitive outputs are returned to the caller but masked in logs.
	Sensitive bool `json:"sensitive,omitempty"`
}

// Outcome is an expected business result other than success. It is part of
// the contract: a caller should handle these codes, not treat them as errors.
type Outcome struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// Step is one recorded action.
type Step struct {
	ID string `json:"id"`
	// Intent is why the step exists, in words, for the human reviewer.
	Intent string        `json:"intent"`
	Action string        `json:"action"`
	Target locate.Target `json:"target"`
	// Value is a template for type/select: literal text and/or
	// {{inputs.name}} / {{secrets.name}} references. Never a concrete secret.
	Value string `json:"value,omitempty"`
	// Output names the declared output an extract step fills.
	Output string      `json:"output,omitempty"`
	Risk   policy.Risk `json:"risk"`
	// Expect is the screen this step must lead to. It is recorded only when
	// the step changed screens during discovery, and is what stops replay
	// from acting on a page that has not arrived yet.
	Expect    *locate.Screen `json:"expect,omitempty"`
	TimeoutMS int            `json:"timeout_ms,omitempty"`
}

// Approval gates unattended use. The digest binds the approval to the exact
// content reviewed: editing an approved artifact silently un-approves it.
type Approval struct {
	Status string `json:"status"`
	By     string `json:"by,omitempty"`
	At     string `json:"at,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// Provenance records where the artifact came from. It deliberately holds the
// goal template and run id, not the model transcript or any concrete values.
type Provenance struct {
	RecordedAt    string `json:"recorded_at"`
	Goal          string `json:"goal"`
	Model         string `json:"model"`
	Backend       string `json:"backend"`
	RunID         string `json:"run_id"`
	Tenant        string `json:"recorded_on_tenant"`
	HumanAssisted bool   `json:"human_assisted"`
}

// Load reads and validates an artifact.
func Load(path string) (*Artifact, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Artifact
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &a, nil
}

// Save writes the artifact as indented JSON, the form a reviewer reads.
func (a *Artifact) Save(path string) error {
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Digest is a hash of everything except the approval block.
func (a *Artifact) Digest() string {
	cp := *a
	cp.Approval = Approval{}
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Approved reports whether the artifact carries an approval of its current
// content.
func (a *Artifact) Approved() bool {
	return a.Approval.Status == StatusApproved && a.Approval.Digest == a.Digest()
}

// MaxRisk is the highest risk among the steps.
func (a *Artifact) MaxRisk() policy.Risk {
	risk := policy.RiskRead
	for _, s := range a.Steps {
		switch s.Risk {
		case policy.RiskIrreversible:
			return policy.RiskIrreversible
		case policy.RiskReversible:
			risk = policy.RiskReversible
		}
	}
	return risk
}

var refPattern = regexp.MustCompile(`\{\{\s*(inputs|secrets)\.([a-zA-Z0-9_]+)\s*\}\}`)

// Validate checks the artifact is internally consistent: every reference
// resolves, every declared output is produced, nothing is ambiguous.
func (a *Artifact) Validate() error {
	if a.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %q (this build reads %q)", a.SchemaVersion, SchemaVersion)
	}
	if a.ID == "" || a.Version == "" {
		return fmt.Errorf("id and version are required")
	}
	if len(a.Steps) == 0 {
		return fmt.Errorf("artifact has no steps")
	}
	inputs := map[string]bool{}
	for _, p := range a.Inputs {
		if inputs[p.Name] {
			return fmt.Errorf("duplicate input %q", p.Name)
		}
		inputs[p.Name] = true
		if !slices.Contains([]string{"string", "integer", "money", "enum"}, p.Type) {
			return fmt.Errorf("input %q: unknown type %q", p.Name, p.Type)
		}
		if p.Pattern != "" {
			if _, err := regexp.Compile(p.Pattern); err != nil {
				return fmt.Errorf("input %q: pattern: %w", p.Name, err)
			}
		}
	}
	outputs := map[string]bool{}
	for _, o := range a.Outputs {
		outputs[o.Name] = false
		if !slices.Contains([]string{"string", "integer", "money"}, o.Type) {
			return fmt.Errorf("output %q: unknown type %q", o.Name, o.Type)
		}
	}
	ids := map[string]bool{}
	for _, s := range a.Steps {
		if s.ID == "" || ids[s.ID] {
			return fmt.Errorf("step id %q missing or duplicated", s.ID)
		}
		ids[s.ID] = true
		if len(s.Target.Locators) == 0 {
			return fmt.Errorf("step %s: no locators", s.ID)
		}
		switch s.Action {
		case ActionClick, ActionType, ActionSelect:
		case ActionExtract:
			if _, ok := outputs[s.Output]; !ok {
				return fmt.Errorf("step %s: extracts undeclared output %q", s.ID, s.Output)
			}
			outputs[s.Output] = true
		default:
			return fmt.Errorf("step %s: unknown action %q", s.ID, s.Action)
		}
		for _, text := range s.templated() {
			for _, m := range refPattern.FindAllStringSubmatch(text, -1) {
				if m[1] == "inputs" && !inputs[m[2]] {
					return fmt.Errorf("step %s: references undeclared input %q", s.ID, m[2])
				}
				if m[1] == "secrets" && !slices.Contains(a.Secrets, m[2]) {
					return fmt.Errorf("step %s: references undeclared secret %q", s.ID, m[2])
				}
			}
		}
	}
	for _, text := range conditionTexts(a.Success) {
		for _, m := range refPattern.FindAllStringSubmatch(text, -1) {
			if m[1] != "inputs" || !inputs[m[2]] {
				return fmt.Errorf("success condition references %s.%s, which is not a declared input", m[1], m[2])
			}
		}
	}
	for name, produced := range outputs {
		if !produced {
			return fmt.Errorf("output %q is declared but no step extracts it", name)
		}
	}
	if a.Risk != a.MaxRisk() {
		return fmt.Errorf("risk is %q but the steps make it %q", a.Risk, a.MaxRisk())
	}
	return nil
}

// templated lists every string in a step that may hold {{...}} references.
func (s *Step) templated() []string {
	out := []string{s.Value}
	for _, l := range s.Target.Locators {
		out = append(out, l.Name, l.Label, l.Row, l.Column)
	}
	if s.Expect != nil {
		out = append(out, s.Expect.Path, s.Expect.Title)
	}
	return out
}

// conditionTexts lists every string in a condition that may hold references.
func conditionTexts(c locate.Condition) []string {
	out := []string{c.Text}
	if c.Screen != nil {
		out = append(out, c.Screen.Path, c.Screen.Title)
	}
	for _, sub := range c.All {
		out = append(out, conditionTexts(sub)...)
	}
	for _, sub := range c.Any {
		out = append(out, conditionTexts(sub)...)
	}
	return out
}
