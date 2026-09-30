package capability

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	inputs := map[string]string{"member_id": "10042", "memo": "{{secrets.pw}}"}
	secrets := map[string]string{"pw": "s3cr3t"}
	cases := []struct {
		tpl, want, fails string
	}{
		{"{{inputs.member_id}}", "10042", ""},
		{"{{ inputs.member_id }}", "10042", ""},
		{"acct {{inputs.member_id}} / {{secrets.pw}}", "acct 10042 / s3cr3t", ""},
		{"literal only", "literal only", ""},
		// A value is substituted once; a reference inside a value stays text.
		{"{{inputs.memo}}", "{{secrets.pw}}", ""},
		{"{{inputs.nope}}", "", "no value for inputs.nope"},
		{"{{secrets.member_id}}", "", "no value for secrets.member_id"},
	}
	for _, c := range cases {
		got, err := Render(c.tpl, inputs, secrets)
		if c.fails != "" {
			if err == nil || !strings.Contains(err.Error(), c.fails) {
				t.Errorf("Render(%q) = %q, %v; want error %q", c.tpl, got, err, c.fails)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("Render(%q) = %q, %v; want %q", c.tpl, got, err, c.want)
		}
	}
}

func TestUsesSecret(t *testing.T) {
	cases := map[string]bool{
		"{{secrets.operator_password}}":     true,
		"id {{ secrets.operator_id }} here": true,
		"{{inputs.member_id}}":              false,
		"secrets.operator_id":               false,
		"":                                  false,
	}
	for tpl, want := range cases {
		if got := UsesSecret(tpl); got != want {
			t.Errorf("UsesSecret(%q) = %v", tpl, got)
		}
	}
}

func TestLiteralPart(t *testing.T) {
	inputs := []Param{{Name: "member_id", Type: "string"}}
	secrets := []string{"operator_id"}
	cases := []struct {
		tpl, want, fails string
	}{
		{"{{inputs.member_id}}", "", ""},
		{"Acct {{inputs.member_id}}-{{secrets.operator_id}}", "Acct -", ""},
		{"Regular Savings", "Regular Savings", ""},
		{"{{inputs.account}}", "", "not a declared input"},
		{"{{secrets.operator_password}}", "", "not a declared secret"},
		{"{{input.member_id}}", "", "malformed placeholder"},
		{"{{inputs.member_id", "", "malformed placeholder"},
		{"{{inputs.member-id}}", "", "malformed placeholder"},
	}
	for _, c := range cases {
		got, err := LiteralPart(c.tpl, inputs, secrets)
		if c.fails != "" {
			if err == nil || !strings.Contains(err.Error(), c.fails) {
				t.Errorf("LiteralPart(%q) = %q, %v; want error %q", c.tpl, got, err, c.fails)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("LiteralPart(%q) = %q, %v; want %q", c.tpl, got, err, c.want)
		}
	}
}

func TestCheckParams(t *testing.T) {
	params := []Param{
		{Name: "member_id", Type: "string", Pattern: `^[0-9]{1,10}$`},
		{Name: "count", Type: "integer"},
		{Name: "share", Type: "enum", Enum: []string{"savings", "checking"}},
		{Name: "deposit", Type: "money"},
	}
	good := func() map[string]string {
		return map[string]string{"member_id": "10042", "count": "-3", "share": "savings", "deposit": "$1,250.00"}
	}
	if err := CheckParams(params, good()); err != nil {
		t.Fatalf("valid inputs rejected: %v", err)
	}
	cases := []struct {
		name  string
		edit  func(in map[string]string)
		fails string
	}{
		{"missing", func(in map[string]string) { delete(in, "share") }, `missing input "share"`},
		{"unknown", func(in map[string]string) { in["branch"] = "12" }, `unknown input "branch"`},
		{"pattern", func(in map[string]string) { in["member_id"] = "10042; DROP" }, "does not match the required pattern"},
		{"pattern length", func(in map[string]string) { in["member_id"] = "12345678901" }, "does not match the required pattern"},
		{"enum", func(in map[string]string) { in["share"] = "Savings" }, "must be one of: savings, checking"},
		{"integer", func(in map[string]string) { in["count"] = "3.5" }, `"count" must be an integer`},
		{"integer word", func(in map[string]string) { in["count"] = "three" }, "must be an integer"},
		{"money", func(in map[string]string) { in["deposit"] = "12.5" }, `"deposit" must be a dollar amount`},
		{"money text", func(in map[string]string) { in["deposit"] = "lots" }, "must be a dollar amount"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := good()
			c.edit(in)
			err := CheckParams(params, in)
			if err == nil || !strings.Contains(err.Error(), c.fails) {
				t.Fatalf("got %v, want an error mentioning %q", err, c.fails)
			}
		})
	}
}

func TestParseValue(t *testing.T) {
	cases := []struct {
		typ, raw string
		want     any // nil means the value must be rejected
	}{
		{"money", "$5,230.18", "5230.18"},
		{"money", "  $5,230.18 ", "5230.18"},
		{"money", "-$12.00", "-12.00"},
		{"money", "$ 40", "40.00"},
		{"money", "1,000", "1000.00"},
		{"money", "1,234,567.89", "1234567.89"},
		{"money", "0.50", "0.50"},
		{"money", "5230", "5230.00"},
		{"money", "5230.1", nil},
		{"money", "5230.123", nil},
		{"money", "1,00", nil},
		{"money", "12,3456", nil},
		{"money", "$-12.00", nil},
		{"money", "(12.00)", nil},
		{"money", "USD 12.00", nil},
		{"money", "", nil},

		{"integer", "42", int64(42)},
		{"integer", "-7", int64(-7)},
		{"integer", "1,234", int64(1234)},
		{"integer", "12.0", nil},
		{"integer", "", nil},

		{"string", "  Dana R. Whitfield ", "Dana R. Whitfield"},
		{"string", "   ", nil},

		{"date", "2026-09-30", nil},
	}
	for _, c := range cases {
		got, err := ParseValue(c.typ, c.raw)
		if c.want == nil {
			if err == nil {
				t.Errorf("ParseValue(%s, %q) = %#v, want an error", c.typ, c.raw, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseValue(%s, %q) = %#v, %v; want %#v", c.typ, c.raw, got, err, c.want)
		}
	}
}
