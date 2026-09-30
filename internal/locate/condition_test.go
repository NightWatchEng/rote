package locate

import "testing"

func TestConditionEval(t *testing.T) {
	txt := func(s, frame string) Condition { return Condition{Text: s, Frame: frame} }
	screen := func(frame, path, title string) Condition {
		return Condition{Screen: &Screen{Frame: frame, Path: path, Title: title}}
	}
	cases := []struct {
		name   string
		c      Condition
		ok     bool
		detail string
	}{
		{"empty condition always holds", Condition{}, true, ""},
		{"text anywhere", txt("Member Inquiry", ""), true, "Member Inquiry"},
		{"text reports the longest match", txt("member detail", ""), true, "Member Detail loaded for member 10042"},
		{"text is normalized", txt("  MEMBER detail ", "main"), true, "Member Detail loaded for member 10042"},
		{"text substring", txt("Savings", "main"), true, "Regular Savings"},
		{"text in the wrong frame", txt("Member Detail", "nav"), false, ""},
		{"text absent", txt("No member found", ""), false, ""},

		{"screen path and title", screen("main", "/member", "Member Detail"), true, ""},
		{"screen title is normalized", screen("main", "", "member  DETAIL"), true, ""},
		{"screen path ignores host and query", screen("main", "/member", ""), true, ""},
		{"screen wrong path", screen("main", "/inquiry", ""), false, ""},
		{"screen wrong title", screen("main", "/member", "Member Inquiry"), false, ""},
		{"screen unknown frame", screen("popup", "", ""), false, ""},

		{"all holds, detail from first text", Condition{All: []Condition{screen("main", "/member", ""), txt("Regular Savings", "main"), txt("Share Draft", "main")}}, true, "Regular Savings"},
		{"all fails on one", Condition{All: []Condition{txt("Regular Savings", "main"), txt("No member found", "")}}, false, ""},
		{"any holds on the second", Condition{Any: []Condition{txt("No member found", ""), txt("Share Draft", "")}}, true, "Share Draft"},
		{"any fails when none hold", Condition{Any: []Condition{txt("No member found", ""), screen("main", "/inquiry", "")}}, false, ""},
		{"nested", Condition{Any: []Condition{
			{All: []Condition{txt("Access denied", ""), screen("main", "/member", "")}},
			{All: []Condition{screen("nav", "/nav", "Navigation"), txt("Home", "nav")}},
		}}, true, "Home"},
	}
	obs := memberDetail()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, detail := c.c.Eval(obs)
			if ok != c.ok || detail != c.detail {
				t.Fatalf("Eval = (%v, %q), want (%v, %q)", ok, detail, c.ok, c.detail)
			}
		})
	}
}

func TestPathOf(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8080/member?no=10042": "/member",
		"http://127.0.0.1:8081/subaccount/new":  "/subaccount/new",
		"http://127.0.0.1:8080":                 "",
		"/inquiry":                              "/inquiry",
		"about:blank":                           "",
		"http://[::1":                           "http://[::1", // unparseable: returned as-is
	}
	for in, want := range cases {
		if got := PathOf(in); got != want {
			t.Errorf("PathOf(%q) = %q, want %q", in, got, want)
		}
	}
}
