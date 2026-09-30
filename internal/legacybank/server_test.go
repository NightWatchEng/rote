package legacybank

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	opID     = "teller01"
	opPwd    = "pw-test-1"
	override = "7788"
)

// teller is one browser session against the simulator: a cookie jar that
// follows redirects, like the frame the automation drives.
type teller struct {
	t    *testing.T
	c    *http.Client
	base string
}

type page struct {
	status int
	path   string // where redirects ended up
	body   string
}

func start(t *testing.T, v Variant) *teller {
	t.Helper()
	srv := httptest.NewServer(New(Config{Variant: v, OperatorID: opID, OperatorPassword: opPwd, OverrideCode: override}))
	t.Cleanup(srv.Close)
	return newTeller(t, srv.URL)
}

func newTeller(t *testing.T, base string) *teller {
	jar, _ := cookiejar.New(nil)
	return &teller{t: t, c: &http.Client{Jar: jar}, base: base}
}

func (b *teller) do(req *http.Request, err error) page {
	b.t.Helper()
	if err != nil {
		b.t.Fatal(err)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return page{resp.StatusCode, resp.Request.URL.Path, string(raw)}
}

func (b *teller) get(path string) page {
	b.t.Helper()
	return b.do(http.NewRequest(http.MethodGet, b.base+path, nil))
}

func (b *teller) post(path string, form url.Values) page {
	b.t.Helper()
	req, err := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	if err == nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return b.do(req, err)
}

func (b *teller) signOn() {
	b.t.Helper()
	p := b.post("/signon", url.Values{"opid": {opID}, "pwd": {opPwd}})
	mustContain(b.t, p, "<title>Workstation Home</title>")
}

func (b *teller) fault(kv ...string) {
	b.t.Helper()
	form := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	if p := b.post("/_sim/fault", form); p.status != http.StatusOK {
		b.t.Fatalf("arming fault %v: %d %s", kv, p.status, p.body)
	}
}

func mustContain(t *testing.T, p page, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(p.body, w) {
			t.Fatalf("page %s (HTTP %d) lacks %q:\n%s", p.path, p.status, w, p.body)
		}
	}
}

func mustNotContain(t *testing.T, p page, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(p.body, w) {
			t.Fatalf("page %s contains %q", p.path, w)
		}
	}
}

func pinecrest(t *testing.T) *teller { return start(t, Variants["pinecrest"]) }

func TestSignOn(t *testing.T) {
	b := pinecrest(t)
	p := b.get("/home")
	mustContain(t, p, "<title>Operator Sign On</title>", `name="opid"`, `type="password" name="pwd"`)
	mustNotContain(t, p, "Workstation Home")

	p = b.post("/signon", url.Values{"opid": {opID}, "pwd": {"wrong"}})
	mustContain(t, p, "Invalid operator ID or password.")

	b.signOn()
	mustContain(t, b.get("/home"), "Operator <b>teller01</b> is signed on.")

	mustContain(t, b.get("/signoff"), "You have signed off.")
	mustContain(t, b.get("/home"), "<title>Operator Sign On</title>")
}

func TestMemberInquiry(t *testing.T) {
	b := pinecrest(t)
	b.signOn()

	p := b.post("/inquiry/search", url.Values{"memberno": {" 10042 "}})
	if p.path != "/member" {
		t.Fatalf("search ended at %s, want /member", p.path)
	}
	mustContain(t, p, "<title>Member Detail</title>", "Dana R. Whitfield", "000-45-6789",
		"Share Draft Checking", "$5,230.18", "$1,104.55", `href="/subaccount/new?no=10042"`)

	p = b.post("/inquiry/search", url.Values{"memberno": {"99999"}})
	mustContain(t, p, "No member found matching number 99999.", `value="99999"`)

	p = b.post("/inquiry/search", url.Values{"memberno": {"10a42"}})
	mustContain(t, p, "Validation Error: Member number must be numeric.")

	for _, p := range []page{
		b.post("/inquiry/search", url.Values{"memberno": {"10077"}}),
		b.get("/member?no=10077"),
	} {
		mustContain(t, p, "<title>Access Denied</title>", "Access denied (SEC-403).", "Operator teller01 is not authorized")
		mustNotContain(t, p, "000-00-0000")
	}
}

func TestOpenSubAccount(t *testing.T) {
	b := pinecrest(t)
	b.signOn()

	mustContain(t, b.get("/subaccount/new?no=10063"), "<title>Open Sub-Account</title>", "Priya N. Raman",
		`<option value="Money Market">`)

	review := func(typ, deposit string) page {
		return b.post("/subaccount/review", url.Values{"no": {"10063"}, "type": {typ}, "nickname": {"Rainy Day"}, "deposit": {deposit}})
	}
	mustContain(t, review("", "100.00"), "Validation Error: Account type is required.")
	p := review("Money Market", "100.00")
	// The form is re-shown with what was entered, so a person can correct it.
	mustContain(t, p, "Validation Error: Opening deposit is below the $2,500.00 minimum for Money Market.",
		`<option value="Money Market" selected>`, `value="100.00"`, `value="Rainy Day"`)
	mustContain(t, review("Regular Savings", "lots"), "Validation Error: Opening deposit must be a dollar amount.")

	p = review("Regular Savings", "$250")
	if p.path != "/subaccount/review" {
		t.Fatalf("valid form ended at %s", p.path)
	}
	mustContain(t, p, "<title>Review New Sub-Account</title>", "Regular Savings", "Rainy Day", "$250.00",
		`action="/subaccount/confirm"`)

	p = b.post("/subaccount/confirm", nil)
	mustContain(t, p, "<title>Sub-Account Opened</title>", "The sub-account has been opened.",
		"New Suffix:</td><td bgcolor=\"#FFFFFF\">0003", "MC-481201")

	mustContain(t, b.get("/member?no=10063"),
		`<td>0003</td><td>Regular Savings</td><td align="right">$250.00</td>`)

	// The pending application is consumed: confirming again opens nothing.
	if p := b.post("/subaccount/confirm", nil); p.path != "/inquiry" {
		t.Fatalf("second confirm ended at %s, want /inquiry", p.path)
	}
	mustNotContain(t, b.get("/member?no=10063"), "<td>0004</td>")
}

func TestLargeDepositRequiresOverride(t *testing.T) {
	b := pinecrest(t)
	b.signOn()

	// Just under the threshold goes straight to review.
	p := b.post("/subaccount/review", url.Values{"no": {"10042"}, "type": {"Share Certificate (12 mo)"}, "deposit": {"9,999.99"}})
	mustContain(t, p, "<title>Review New Sub-Account</title>")

	p = b.post("/subaccount/review", url.Values{"no": {"10042"}, "type": {"Share Certificate (12 mo)"}, "deposit": {"10,000.00"}})
	if p.path != "/subaccount/override" {
		t.Fatalf("$10,000 deposit ended at %s, want /subaccount/override", p.path)
	}
	mustContain(t, p, "<title>Supervisor Override Required</title>", "$10,000.00 or more", `type="password" name="code"`)

	// Neither the review nor the confirmation can be reached around it.
	if p := b.get("/subaccount/review"); p.path != "/subaccount/override" {
		t.Fatalf("review without override ended at %s", p.path)
	}
	if p := b.post("/subaccount/confirm", nil); p.path != "/inquiry" {
		t.Fatalf("confirm without override ended at %s, want /inquiry", p.path)
	}
	mustNotContain(t, b.get("/member?no=10042"), "<td>0004</td>")

	mustContain(t, b.post("/subaccount/override", url.Values{"code": {"0000"}}), "Override code rejected.")
	if p := b.post("/subaccount/confirm", nil); p.path != "/inquiry" {
		t.Fatalf("confirm after a rejected code ended at %s", p.path)
	}

	p = b.post("/subaccount/override", url.Values{"code": {override}})
	if p.path != "/subaccount/review" {
		t.Fatalf("accepted override ended at %s", p.path)
	}
	mustContain(t, p, "<title>Review New Sub-Account</title>", "$10,000.00")
	mustContain(t, b.post("/subaccount/confirm", nil), "<title>Sub-Account Opened</title>", "0004")
	mustContain(t, b.get("/member?no=10042"), `<td>0004</td><td>Share Certificate (12 mo)</td><td align="right">$10,000.00</td>`)
}

func TestNoticeFault(t *testing.T) {
	b := pinecrest(t)
	b.signOn()
	b.fault("kind", "notice")

	// An interstitial only stands in for a page load: the POST goes through
	// and the notice lands on the member page it redirects to, carrying the
	// query so OK continues where the operator was going.
	p := b.post("/inquiry/search", url.Values{"memberno": {"10042"}})
	mustContain(t, p, "<title>System Notice</title>", "End-of-day processing", `action="/member"`,
		`<input type="hidden" name="no" value="10042">`, `value="OK"`)
	mustContain(t, b.get("/member?no=10042"), "Dana R. Whitfield")
}

func TestErrorFaultAfterAndCount(t *testing.T) {
	b := pinecrest(t)
	b.signOn()
	b.fault("kind", "error", "after", "1", "count", "2")

	seq := []int{200, 500, 500, 200}
	for i, want := range seq {
		p := b.get("/home")
		if p.status != want {
			t.Fatalf("request %d: HTTP %d, want %d", i+1, p.status, want)
		}
		if want == 500 {
			mustContain(t, p, "Application Error", "MC-5000")
		}
	}
}

func TestSlowFault(t *testing.T) {
	b := pinecrest(t)
	b.signOn()
	b.fault("kind", "slow", "delay_ms", "200")
	begin := time.Now()
	mustContain(t, b.get("/home"), "Workstation Home")
	if d := time.Since(begin); d < 200*time.Millisecond {
		t.Fatalf("slow fault returned after %v", d)
	}
	begin = time.Now()
	b.get("/home")
	if d := time.Since(begin); d >= 200*time.Millisecond {
		t.Fatalf("slow fault fired twice (%v)", d)
	}
}

func TestExpireFault(t *testing.T) {
	b := pinecrest(t)
	b.signOn()
	b.fault("kind", "expire")

	mustContain(t, b.get("/inquiry"), "<title>Session Expired</title>", "Your session has expired. Please sign on again.")
	// The dead cookie was dropped: the next load is an ordinary sign-on.
	p := b.get("/inquiry")
	mustContain(t, p, "<title>Operator Sign On</title>")
	mustNotContain(t, p, "expired")

	b.signOn()
	mustContain(t, b.get("/inquiry"), "Member Number:")
}

func TestFaultControlPlaneRejectsBadInput(t *testing.T) {
	b := pinecrest(t)
	if p := b.post("/_sim/fault", url.Values{"kind": {"meteor"}}); p.status != http.StatusBadRequest {
		t.Errorf("unknown fault kind: HTTP %d", p.status)
	}
	if p := b.get("/_sim/fault"); p.status != http.StatusMethodNotAllowed {
		t.Errorf("GET /_sim/fault: HTTP %d", p.status)
	}
	if p := b.get("/_sim/reset"); p.status != http.StatusMethodNotAllowed {
		t.Errorf("GET /_sim/reset: HTTP %d", p.status)
	}
}

func TestReset(t *testing.T) {
	b := pinecrest(t)
	b.signOn()
	b.post("/subaccount/review", url.Values{"no": {"10063"}, "type": {"Holiday Club"}, "deposit": {"5.00"}})
	mustContain(t, b.post("/subaccount/confirm", nil), "MC-481201")
	b.fault("kind", "error", "after", "5")

	if p := b.post("/_sim/reset", nil); p.status != http.StatusOK {
		t.Fatalf("reset: HTTP %d", p.status)
	}
	var st struct {
		Sessions      int               `json:"sessions"`
		Faults        []json.RawMessage `json:"faults"`
		Confirmations int               `json:"confirmations"`
	}
	if err := json.Unmarshal([]byte(b.get("/_sim/state").body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Sessions != 0 || len(st.Faults) != 0 || st.Confirmations != 0 {
		t.Fatalf("state after reset = %+v", st)
	}

	mustContain(t, b.get("/home"), "Your session has expired")
	b.signOn()
	p := b.get("/member?no=10063")
	mustContain(t, p, "Priya N. Raman")
	mustNotContain(t, p, "Holiday Club")

	// Confirmation numbering restarts too.
	b.post("/subaccount/review", url.Values{"no": {"10063"}, "type": {"Holiday Club"}, "deposit": {"5.00"}})
	mustContain(t, b.post("/subaccount/confirm", nil), "MC-481201", "0003")
}

func TestVariantsRenderTheirOwnLabels(t *testing.T) {
	for name, v := range Variants {
		t.Run(name, func(t *testing.T) {
			other := Variants["lakeshore"]
			if name == "lakeshore" {
				other = Variants["pinecrest"]
			}
			b := start(t, v)
			mustContain(t, b.get("/"), `<frame name="banner"`, `<frame name="nav"`, `<frame name="main" src="/home">`)
			mustContain(t, b.get("/banner"), v.Institution, "v"+v.Version, v.BannerColor)
			mustContain(t, b.get("/nav"), v.InquiryNav, "Rate Sheet", "Sign Off")
			mustNotContain(t, b.get("/nav"), other.InquiryNav)

			b.signOn()
			p := b.get("/inquiry")
			mustContain(t, p, "<title>"+v.InquiryNav+"</title>", v.MemberNoLabel, `value="`+v.SearchLabel+`"`)
			mustNotContain(t, p, other.MemberNoLabel)
			// Same flows underneath the relabelling.
			mustContain(t, b.post("/inquiry/search", url.Values{"memberno": {"10058"}}), "Marcus T. Oyelaran")
		})
	}
}
