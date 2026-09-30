package capability

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NightWatchEng/rote/internal/locate"
	"github.com/NightWatchEng/rote/internal/policy"
	"github.com/NightWatchEng/rote/internal/surface"
)

func byLabel(label string) locate.Target {
	return locate.Target{Frame: "main", Locators: []locate.Locator{
		{Strategy: locate.ByLabel, Role: surface.RoleTextbox, Label: label, Side: "left"},
		{Strategy: locate.ByOrdinal, Role: surface.RoleTextbox, Index: 1},
	}}
}

func byName(role, name string) locate.Target {
	return locate.Target{Frame: "main", Locators: []locate.Locator{{Strategy: locate.ByName, Role: role, Name: name}}}
}

// lookup is a small, valid capability: sign on, search a member, read a
// balance out of the accounts table.
func lookup() *Artifact {
	return &Artifact{
		SchemaVersion: SchemaVersion,
		ID:            "member.get_savings_balance",
		Version:       "1.0.0",
		Title:         "Get a member's savings balance",
		App:           AppRef{Profile: "meridian-core", ProfileVersion: "1", Surface: "web"},
		Inputs:        []Param{{Name: "member_id", Type: "string", Pattern: `^[0-9]{1,10}$`}},
		Secrets:       []string{"operator_password"},
		Outputs:       []Output{{Name: "savings_balance", Type: "money"}},
		Outcomes:      []Outcome{{Code: "record_not_found"}},
		Entry:         "/",
		Steps: []Step{
			{ID: "s1", Action: ActionType, Target: byLabel("Password:"), Value: "{{secrets.operator_password}}", Risk: policy.RiskReversible},
			{ID: "s2", Action: ActionType, Target: byLabel("Member Number:"), Value: "{{ inputs.member_id }}", Risk: policy.RiskReversible},
			{ID: "s3", Action: ActionClick, Target: byName(surface.RoleButton, "Search"), Risk: policy.RiskReversible,
				Expect: &locate.Screen{Frame: "main", Path: "/member", Title: "Member Detail"}},
			{ID: "s4", Action: ActionExtract, Output: "savings_balance", Risk: policy.RiskRead, Target: locate.Target{Frame: "main",
				Locators: []locate.Locator{{Strategy: locate.ByGrid, Role: surface.RoleText, Row: "Regular Savings", Column: "Current Balance"}}}},
		},
		Success:  locate.Condition{Text: "Member Detail", Frame: "main"},
		Risk:     policy.RiskReversible,
		Approval: Approval{Status: StatusDraft},
	}
}

func TestValidate(t *testing.T) {
	if err := lookup().Validate(); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(a *Artifact)
		want   string
	}{
		{"schema version", func(a *Artifact) { a.SchemaVersion = "2.0" }, "unsupported schema_version"},
		{"missing id", func(a *Artifact) { a.ID = "" }, "id and version are required"},
		{"no steps", func(a *Artifact) { a.Steps = nil }, "no steps"},
		{"duplicate input", func(a *Artifact) { a.Inputs = append(a.Inputs, a.Inputs[0]) }, `duplicate input "member_id"`},
		{"unknown input type", func(a *Artifact) { a.Inputs[0].Type = "float" }, `unknown type "float"`},
		{"bad input pattern", func(a *Artifact) { a.Inputs[0].Pattern = "([0-9]" }, "pattern"},
		{"unknown output type", func(a *Artifact) { a.Outputs[0].Type = "enum" }, `unknown type "enum"`},
		{"duplicate step id", func(a *Artifact) { a.Steps[2].ID = "s1" }, `step id "s1" missing or duplicated`},
		{"missing step id", func(a *Artifact) { a.Steps[0].ID = "" }, "missing or duplicated"},
		{"step without locators", func(a *Artifact) { a.Steps[2].Target.Locators = nil }, "s3: no locators"},
		{"unknown action", func(a *Artifact) { a.Steps[2].Action = "hover" }, `unknown action "hover"`},
		{"undeclared input in value", func(a *Artifact) { a.Steps[1].Value = "{{inputs.account}}" }, `undeclared input "account"`},
		{"undeclared input in locator", func(a *Artifact) { a.Steps[3].Target.Locators[0].Row = "{{inputs.share_type}}" }, `undeclared input "share_type"`},
		{"undeclared secret in value", func(a *Artifact) { a.Steps[0].Value = "{{secrets.operator_id}}" }, `undeclared secret "operator_id"`},
		{"undeclared secret in expect", func(a *Artifact) { a.Steps[2].Expect.Title = "{{secrets.token}}" }, `undeclared secret "token"`},
		{"extracts undeclared output", func(a *Artifact) { a.Steps[3].Output = "checking_balance" }, `extracts undeclared output "checking_balance"`},
		{"declared output never extracted", func(a *Artifact) {
			a.Outputs = append(a.Outputs, Output{Name: "member_name", Type: "string"})
		}, `output "member_name" is declared but no step extracts it`},
		{"risk understated", func(a *Artifact) { a.Risk = policy.RiskRead }, `risk is "read" but the steps make it "reversible"`},
		{"risk overstated", func(a *Artifact) { a.Risk = policy.RiskIrreversible }, "risk is"},
		{"risk hidden by a step", func(a *Artifact) { a.Steps[2].Risk = policy.RiskIrreversible }, `the steps make it "irreversible"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := lookup()
			c.mutate(a)
			err := a.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, c.want)
			}
		})
	}
}

func TestMaxRisk(t *testing.T) {
	steps := func(rs ...policy.Risk) *Artifact {
		a := &Artifact{}
		for _, r := range rs {
			a.Steps = append(a.Steps, Step{Risk: r})
		}
		return a
	}
	cases := []struct {
		a    *Artifact
		want policy.Risk
	}{
		{steps(), policy.RiskRead},
		{steps(policy.RiskRead), policy.RiskRead},
		{steps(policy.RiskRead, policy.RiskReversible, policy.RiskRead), policy.RiskReversible},
		{steps(policy.RiskIrreversible, policy.RiskReversible), policy.RiskIrreversible},
		{steps(policy.RiskRead, policy.RiskReversible, policy.RiskIrreversible), policy.RiskIrreversible},
	}
	for i, c := range cases {
		if got := c.a.MaxRisk(); got != c.want {
			t.Errorf("case %d: %s, want %s", i, got, c.want)
		}
	}
}

func approve(a *Artifact) {
	a.Approval = Approval{Status: StatusApproved, By: "reviewer", At: "2026-09-30T17:00:00Z", Digest: a.Digest()}
}

func TestApprovalIsBoundToContent(t *testing.T) {
	a := lookup()
	d := a.Digest()
	if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
		t.Fatalf("digest %q", d)
	}
	if a.Approved() {
		t.Fatal("a draft is not approved")
	}
	approve(a)
	if !a.Approved() {
		t.Fatal("approval of the current content not recognised")
	}
	if a.Digest() != d {
		t.Fatal("the approval block must not feed the digest")
	}

	edits := map[string]func(a *Artifact){
		"title":           func(a *Artifact) { a.Title += "!" },
		"step value":      func(a *Artifact) { a.Steps[1].Value = "10042" },
		"locator":         func(a *Artifact) { a.Steps[2].Target.Locators[0].Name = "Find" },
		"success":         func(a *Artifact) { a.Success.Text = "Member" },
		"risk":            func(a *Artifact) { a.Steps[2].Risk = policy.RiskIrreversible },
		"input pattern":   func(a *Artifact) { a.Inputs[0].Pattern = ".*" },
		"new secret":      func(a *Artifact) { a.Secrets = append(a.Secrets, "operator_id") },
		"profile version": func(a *Artifact) { a.App.ProfileVersion = "2" },
	}
	for name, edit := range edits {
		a := lookup()
		approve(a)
		edit(a)
		if a.Approved() {
			t.Errorf("editing the %s kept the approval", name)
		}
		if !strings.Contains(Describe(a), "approval VOID") {
			t.Errorf("editing the %s: reviewer sheet does not flag the void approval", name)
		}
	}

	a = lookup()
	approve(a)
	a.Approval.Status = StatusDraft
	if a.Approved() {
		t.Fatal("a matching digest without approved status is not an approval")
	}
}

func TestLoadRealArtifact(t *testing.T) {
	a, err := Load(filepath.Join("..", "..", "artifacts", "member.get_savings_balance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "member.get_savings_balance" || a.Risk != a.MaxRisk() || len(a.Steps) == 0 {
		t.Fatalf("unexpected artifact: %s risk %s, %d steps", a.ID, a.Risk, len(a.Steps))
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	a := lookup()
	approve(a)
	path := filepath.Join(t.TempDir(), "a.json")
	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || !b.Approved() {
		t.Fatal("artifact changed across save and load")
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	a := lookup()
	a.Risk = policy.RiskRead
	path := filepath.Join(t.TempDir(), "a.json")
	if err := a.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "risk is") {
		t.Fatalf("Load accepted an invalid artifact: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file")
	}
}

func TestTool(t *testing.T) {
	a := lookup()
	a.Inputs = append(a.Inputs,
		Param{Name: "count", Type: "integer", Description: "how many"},
		Param{Name: "share", Type: "enum", Enum: []string{"savings", "checking"}},
		Param{Name: "amount", Type: "money"},
	)
	tool := a.Tool()
	if tool.Name != a.ID || tool.Version != a.Version || tool.Risk != "reversible" || tool.Approved {
		t.Fatalf("header: %+v", tool)
	}
	props := tool.InputSchema["properties"].(map[string]any)
	prop := func(name string) map[string]any { return props[name].(map[string]any) }
	if prop("member_id")["type"] != "string" || prop("member_id")["pattern"] != `^[0-9]{1,10}$` {
		t.Errorf("member_id: %v", prop("member_id"))
	}
	if prop("count")["type"] != "integer" || prop("count")["description"] != "how many" {
		t.Errorf("count: %v", prop("count"))
	}
	if !reflect.DeepEqual(prop("share")["enum"], []string{"savings", "checking"}) {
		t.Errorf("share: %v", prop("share"))
	}
	// Money travels as a string so no float ever touches it.
	if prop("amount")["type"] != "string" {
		t.Errorf("amount: %v", prop("amount"))
	}
	if !reflect.DeepEqual(tool.InputSchema["required"], []string{"member_id", "count", "share", "amount"}) {
		t.Errorf("required: %v", tool.InputSchema["required"])
	}
	if tool.InputSchema["additionalProperties"] != false {
		t.Error("unknown inputs must be refused by the schema")
	}
	if len(tool.Returns) != 1 || tool.Returns[0].Name != "savings_balance" || len(tool.Outcomes) != 1 {
		t.Errorf("returns %v outcomes %v", tool.Returns, tool.Outcomes)
	}

	approve(a)
	if !a.Tool().Approved {
		t.Error("tool does not report approval")
	}
}

func TestDescribe(t *testing.T) {
	a := lookup()
	a.Steps[2].Risk = policy.RiskIrreversible
	a.Risk = policy.RiskIrreversible
	approve(a)
	sheet := Describe(a)
	for _, want := range []string{
		"approved by reviewer",
		"member_id: string pattern ^[0-9]{1,10}$",
		"- operator_password",
		"savings_balance: money",
		"- record_not_found",
		"3. click  [IRREVERSIBLE]",
		`button named "Search"`,
		"extract -> savings_balance",
		`text "Member Detail" visible in frame main`,
	} {
		if !strings.Contains(sheet, want) {
			t.Errorf("reviewer sheet lacks %q:\n%s", want, sheet)
		}
	}
	if strings.Contains(sheet, "VOID") {
		t.Error("a current approval is not void")
	}
}
