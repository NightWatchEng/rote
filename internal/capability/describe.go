package capability

import (
	"fmt"
	"strings"
)

// Describe renders an artifact as the sheet a human reviewer reads before
// approving it: the contract first, then every step with the reason it
// exists and exactly how its control will be found.
func Describe(a *Artifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s  (%s @ %s)\n\n%s\n\n", a.Title, a.ID, a.Version, a.Description)
	status := a.Approval.Status
	if a.Approval.Status == StatusApproved && !a.Approved() {
		status = "approval VOID (content changed since it was approved)"
	} else if a.Approved() {
		status = fmt.Sprintf("approved by %s at %s", a.Approval.By, a.Approval.At)
	}
	fmt.Fprintf(&b, "- application: %s v%s via %s\n- risk: %s\n- status: %s\n- recorded: %s on tenant %s by %s (%s), run %s\n",
		a.App.Profile, a.App.ProfileVersion, a.App.Surface, a.Risk, status,
		a.Provenance.RecordedAt, a.Provenance.Tenant, a.Provenance.Model, a.Provenance.Backend, a.Provenance.RunID)
	if a.Provenance.HumanAssisted {
		b.WriteString("- NOTE: a human performed part of the discovery run by hand; those actions are not in the steps below\n")
	}

	b.WriteString("\n## Inputs\n")
	if len(a.Inputs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, p := range a.Inputs {
		extra := ""
		if p.Pattern != "" {
			extra += " pattern " + p.Pattern
		}
		if len(p.Enum) > 0 {
			extra += " one of [" + strings.Join(p.Enum, " | ") + "]"
		}
		if p.Sensitive {
			extra += " (sensitive)"
		}
		fmt.Fprintf(&b, "- %s: %s%s — %s\n", p.Name, p.Type, extra, p.Description)
	}
	fmt.Fprintf(&b, "\n## Secrets (resolved from the tenant binding at run time)\n%s\n", orNone(a.Secrets))

	b.WriteString("\n## Outputs\n")
	if len(a.Outputs) == 0 {
		b.WriteString("(none)\n")
	}
	for _, o := range a.Outputs {
		sens := ""
		if o.Sensitive {
			sens = " (sensitive)"
		}
		fmt.Fprintf(&b, "- %s: %s%s — %s\n", o.Name, o.Type, sens, o.Description)
	}

	b.WriteString("\n## Outcomes other than success\n")
	for _, o := range a.Outcomes {
		fmt.Fprintf(&b, "- %s — %s\n", o.Code, o.Description)
	}

	fmt.Fprintf(&b, "\n## Steps (entry: %s)\n", a.Entry)
	for _, s := range a.Steps {
		verb := s.Action
		switch s.Action {
		case ActionType, ActionSelect:
			verb += " " + s.Value
		case ActionExtract:
			verb += " -> " + s.Output
		}
		risk := ""
		if s.Risk == "irreversible" {
			risk = "  [IRREVERSIBLE]"
		}
		fmt.Fprintf(&b, "%s. %s%s\n    why:    %s\n    target: %s\n", strings.TrimPrefix(s.ID, "s"), verb, risk, s.Intent, s.Target)
		if s.Expect != nil {
			fmt.Fprintf(&b, "    then:   %s\n", s.Expect)
		}
	}
	fmt.Fprintf(&b, "\n## Success checkpoint\n%s\n", a.Success)
	return b.String()
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return "- " + strings.Join(items, "\n- ")
}
