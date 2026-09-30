// Package legacybank is the stand-in target application: a fictional legacy
// core-banking teller workstation ("MeridianCore"). It is deliberately hostile
// to DOM-level automation in the ways real back-office apps are: a frameset,
// nested layout tables, no ids or test ids, form fields whose labels are only
// visually adjacent, and clickable table cells that are not semantic controls.
//
// All data is synthetic. Runtime conditions (not-found, validation errors,
// permission denials, a supervisor-override interstitial, session expiry,
// slowness, server errors) are either data-driven or injected through the
// /_sim/ control plane, which is outside the automation allowlist.
package legacybank

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Variant models one tenant's configuration of the same vendor product:
// different branding and field labels over identical flows.
type Variant struct {
	Name          string
	Institution   string
	Version       string
	BannerColor   string
	MemberNoLabel string
	SearchLabel   string
	InquiryNav    string
}

// Variants are the two tenants the demo runs against.
var Variants = map[string]Variant{
	"pinecrest": {
		Name:          "pinecrest",
		Institution:   "Pinecrest Community Credit Union",
		Version:       "4.2",
		BannerColor:   "#003366",
		MemberNoLabel: "Member Number:",
		SearchLabel:   "Search",
		InquiryNav:    "Member Inquiry",
	},
	"lakeshore": {
		Name:          "lakeshore",
		Institution:   "Lakeshore Federal Credit Union",
		Version:       "4.3",
		BannerColor:   "#5A1E1E",
		MemberNoLabel: "Account Holder No.:",
		SearchLabel:   "Find",
		InquiryNav:    "Member Lookup",
	},
}

// Config is what a tenant deployment of the simulator needs.
type Config struct {
	Variant          Variant
	OperatorID       string
	OperatorPassword string
	OverrideCode     string
}

type account struct {
	Suffix      string
	Description string
	Current     int64 // cents
	Available   int64
	Status      string
}

type member struct {
	Number     string
	Name       string
	SSN        string
	DOB        string
	Address    string
	Phone      string
	Status     string
	Restricted bool
	Accounts   []account
}

type application struct {
	Member     string
	Type       string
	Nickname   string
	Deposit    int64
	Overridden bool
}

type session struct {
	Operator string
	Pending  *application
}

// Fault is one injected runtime condition. It fires on main-frame requests:
// After requests are let through first, then it triggers Count times.
type Fault struct {
	Kind    string `json:"kind"` // notice | slow | error | expire
	After   int    `json:"after"`
	Count   int    `json:"count"`
	DelayMS int    `json:"delay_ms,omitempty"`
}

// Server is the simulator. The zero value is not usable; call New.
type Server struct {
	cfg Config
	mux *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*session
	members  map[string]*member
	faults   []*Fault
	confirms int
}

const cookieName = "MCSESSION"

var accountTypes = []struct {
	Name string
	Min  int64 // minimum opening deposit, cents
}{
	{"Regular Savings", 500},
	{"Holiday Club", 500},
	{"Share Certificate (12 mo)", 50000},
	{"Money Market", 250000},
}

// overrideThreshold is the opening deposit at which a supervisor must
// countersign before the review screen is shown.
const overrideThreshold = 1000000

// New builds a simulator with a fresh copy of the synthetic data.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.reset()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/{$}", s.frameset)
	s.mux.HandleFunc("/banner", s.banner)
	s.mux.HandleFunc("/nav", s.nav)
	s.mux.HandleFunc("/home", s.main(s.home))
	s.mux.HandleFunc("/signon", s.signon)
	s.mux.HandleFunc("/signoff", s.signoff)
	s.mux.HandleFunc("/rates", s.main(s.rates))
	s.mux.HandleFunc("/inquiry", s.main(s.inquiry))
	s.mux.HandleFunc("/inquiry/search", s.main(s.search))
	s.mux.HandleFunc("/member", s.main(s.memberDetail))
	s.mux.HandleFunc("/subaccount/new", s.main(s.subNew))
	s.mux.HandleFunc("/subaccount/review", s.main(s.subReview))
	s.mux.HandleFunc("/subaccount/override", s.main(s.subOverride))
	s.mux.HandleFunc("/subaccount/confirm", s.main(s.subConfirm))
	s.mux.HandleFunc("/_sim/fault", s.simFault)
	s.mux.HandleFunc("/_sim/reset", s.simReset)
	s.mux.HandleFunc("/_sim/state", s.simState)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*session{}
	s.faults = nil
	s.confirms = 0
	s.members = map[string]*member{}
	for _, m := range []member{
		{Number: "10042", Name: "Dana R. Whitfield", SSN: "000-45-6789", DOB: "03/14/1981",
			Address: "418 Alder Way, Pinecrest, OR 97000", Phone: "(503) 555-0142", Status: "Active",
			Accounts: []account{
				{"0001", "Regular Savings", 523018, 523018, "Open"},
				{"0002", "Share Draft Checking", 120455, 110455, "Open"},
				{"0003", "Holiday Club", 31000, 31000, "Open"},
			}},
		{Number: "10058", Name: "Marcus T. Oyelaran", SSN: "000-61-2204", DOB: "11/02/1974",
			Address: "77 Quarry Road, Pinecrest, OR 97000", Phone: "(503) 555-0187", Status: "Active",
			Accounts: []account{
				{"0001", "Regular Savings", 1890244, 1885244, "Open"},
				{"0002", "Share Draft Checking", 6710, 6710, "Open"},
			}},
		{Number: "10063", Name: "Priya N. Raman", SSN: "000-77-9031", DOB: "07/29/1990",
			Address: "9 Ferry Street, Pinecrest, OR 97000", Phone: "(503) 555-0113", Status: "Active",
			Accounts: []account{
				{"0002", "Share Draft Checking", 248090, 248090, "Open"},
			}},
		{Number: "10077", Name: "Staff Account", SSN: "000-00-0000", DOB: "01/01/1970",
			Address: "-", Phone: "-", Status: "Active", Restricted: true},
	} {
		s.members[m.Number] = &m
	}
}

// ---- page chrome ----

func (s *Server) page(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<html><head><title>%s</title></head>
<body bgcolor="#ECE9D8" text="#000000" link="#000080" vlink="#000080">
<table width="100%%" border="0" cellpadding="0" cellspacing="0"><tr><td>
<table width="100%%" border="0" cellpadding="4" cellspacing="0" bgcolor="#C0C0C0"><tr>
<td><font face="Arial" size="3"><b>%s</b></font></td></tr></table>
</td></tr><tr><td>
<table border="0" cellpadding="6" cellspacing="0"><tr><td><font face="Arial" size="2">
%s
</font></td></tr></table>
</td></tr></table>
</body></html>`, html.EscapeString(title), html.EscapeString(title), body)
}

func esc(v string) string { return html.EscapeString(v) }

func money(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	whole := strconv.FormatInt(cents/100, 10)
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := fmt.Sprintf("$%s.%02d", b.String(), cents%100)
	if neg {
		out = "-" + out
	}
	return out
}

var moneyIn = regexp.MustCompile(`^\$?\s*(\d{1,3}(,\d{3})+|\d+)(\.(\d{1,2}))?$`)

func parseMoney(v string) (int64, bool) {
	m := moneyIn.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, false
	}
	whole, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	if err != nil {
		return 0, false
	}
	frac := m[4]
	for len(frac) < 2 {
		frac += "0"
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	return whole*100 + f, true
}

// ---- frames ----

func (s *Server) frameset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html><head><title>MeridianCore Teller Workstation</title></head>
<frameset rows="58,*" border="1">
<frame name="banner" src="/banner" scrolling="no" noresize>
<frameset cols="180,*" border="1">
<frame name="nav" src="/nav">
<frame name="main" src="/home">
</frameset>
</frameset></html>`)
}

func (s *Server) banner(w http.ResponseWriter, r *http.Request) {
	v := s.cfg.Variant
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<html><head><title>Banner</title></head><body bgcolor="%s" text="#FFFFFF" topmargin="6">
<table width="100%%" border="0" cellpadding="2" cellspacing="0"><tr>
<td><font face="Arial" size="4" color="#FFFFFF"><b>%s</b></font></td>
<td align="right"><font face="Arial" size="2" color="#FFFFFF">MeridianCore Teller Workstation v%s</font></td>
</tr></table></body></html>`, v.BannerColor, esc(v.Institution), esc(v.Version))
}

func (s *Server) nav(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	item := func(label, href string) string {
		// A clickable table cell, not a link or button: common in apps of this era.
		return fmt.Sprintf(`<tr><td bgcolor="#D4D0C8" style="cursor:pointer" onmouseover="this.bgColor='#FFFFCC'" onmouseout="this.bgColor='#D4D0C8'" onclick="parent.frames['main'].location.href='%s'"><font face="Arial" size="2">%s</font></td></tr>`, href, esc(label))
	}
	fmt.Fprintf(w, `<html><head><title>Menu</title></head><body bgcolor="#808080" topmargin="8">
<table width="100%%" border="0" cellpadding="1" cellspacing="0" bgcolor="#000000"><tr><td>
<table width="100%%" border="0" cellpadding="5" cellspacing="1">
<tr><td bgcolor="#000080"><font face="Arial" size="2" color="#FFFFFF"><b>Main Menu</b></font></td></tr>
%s%s%s
</table></td></tr></table></body></html>`,
		item(s.cfg.Variant.InquiryNav, "/inquiry"), item("Rate Sheet", "/rates"), item("Sign Off", "/signoff"))
}

// ---- session + fault middleware for main-frame pages ----

type mainHandler func(w http.ResponseWriter, r *http.Request, sess *session)

func (s *Server) main(h mainHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.applyFaults(w, r) {
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil {
			s.signonPage(w, http.StatusOK, "Operator Sign On", "")
			return
		}
		s.mu.Lock()
		sess := s.sessions[c.Value]
		s.mu.Unlock()
		if sess == nil {
			// Drop the dead cookie so the next load is an ordinary sign-on.
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
			s.signonPage(w, http.StatusOK, "Session Expired",
				`<font color="#CC0000"><b>Your session has expired. Please sign on again.</b></font><br><br>`)
			return
		}
		h(w, r, sess)
	}
}

// applyFaults triggers at most one armed fault and reports whether it wrote
// the response.
func (s *Server) applyFaults(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	var fire *Fault
	for _, f := range s.faults {
		if f.Count <= 0 {
			continue
		}
		if f.Kind == "notice" && r.Method != http.MethodGet {
			continue // an interstitial can only stand in for a page load
		}
		if f.After > 0 {
			f.After--
			continue
		}
		f.Count--
		fire = f
		break
	}
	if fire != nil && fire.Kind == "expire" {
		s.sessions = map[string]*session{}
	}
	s.mu.Unlock()

	if fire == nil {
		return false
	}
	switch fire.Kind {
	case "slow":
		time.Sleep(time.Duration(fire.DelayMS) * time.Millisecond)
	case "error":
		s.page(w, http.StatusInternalServerError, "Application Error",
			`<font color="#CC0000"><b>Application Error</b></font><br><br>The request could not be completed (MC-5000). Contact your system administrator.`)
		return true
	case "notice":
		s.page(w, http.StatusOK, "System Notice", fmt.Sprintf(
			`<table border="1" cellpadding="8" cellspacing="0" bgcolor="#FFFFE0"><tr><td><font face="Arial" size="2">
<b>System Notice</b><br><br>End-of-day processing begins at 6:00 PM. Transactions posted after the cutoff are dated the next business day.<br><br>
<form method="get" action="%s">%s<input type="submit" value="OK"></form></font></td></tr></table>`,
			esc(r.URL.Path), hiddenQuery(r.URL.Query())))
		return true
	}
	return false
}

func hiddenQuery(q url.Values) string {
	var b strings.Builder
	for k, vs := range q {
		for _, v := range vs {
			fmt.Fprintf(&b, `<input type="hidden" name="%s" value="%s">`, esc(k), esc(v))
		}
	}
	return b.String()
}

// ---- sign on ----

func (s *Server) signonPage(w http.ResponseWriter, status int, title, notice string) {
	s.page(w, status, title, notice+`<form method="post" action="/signon">
<table border="0" cellpadding="3" cellspacing="0">
<tr><td align="right"><font face="Arial" size="2">Operator ID:</font></td><td><input type="text" name="opid" size="14" maxlength="12"></td></tr>
<tr><td align="right"><font face="Arial" size="2">Password:</font></td><td><input type="password" name="pwd" size="14"></td></tr>
<tr><td></td><td><input type="submit" value="Sign On"></td></tr>
</table></form>`)
}

func (s *Server) signon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/home", http.StatusSeeOther)
		return
	}
	if r.FormValue("opid") != s.cfg.OperatorID || r.FormValue("pwd") != s.cfg.OperatorPassword {
		s.signonPage(w, http.StatusOK, "Operator Sign On",
			`<font color="#CC0000"><b>Invalid operator ID or password.</b></font><br><br>`)
		return
	}
	buf := make([]byte, 16)
	rand.Read(buf)
	id := hex.EncodeToString(buf)
	s.mu.Lock()
	s.sessions[id] = &session{Operator: s.cfg.OperatorID}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true})
	http.Redirect(w, r, "/home", http.StatusSeeOther)
}

func (s *Server) signoff(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	s.signonPage(w, http.StatusOK, "Operator Sign On", "You have signed off.<br><br>")
}

// ---- pages ----

func (s *Server) home(w http.ResponseWriter, r *http.Request, sess *session) {
	s.page(w, http.StatusOK, "Workstation Home", fmt.Sprintf(
		`Operator <b>%s</b> is signed on.<br><br>Select a function from the Main Menu.`, esc(sess.Operator)))
}

func (s *Server) rates(w http.ResponseWriter, r *http.Request, sess *session) {
	s.page(w, http.StatusOK, "Rate Sheet", `<table border="1" cellpadding="4" cellspacing="0">
<tr bgcolor="#C0C0C0"><td><b>Product</b></td><td><b>APY</b></td></tr>
<tr><td>Regular Savings</td><td align="right">0.45%</td></tr>
<tr><td>Money Market</td><td align="right">2.10%</td></tr>
<tr><td>Share Certificate (12 mo)</td><td align="right">4.05%</td></tr></table>`)
}

func (s *Server) inquiryForm(w http.ResponseWriter, title, notice, value string) {
	v := s.cfg.Variant
	s.page(w, http.StatusOK, title, notice+fmt.Sprintf(`<form method="post" action="/inquiry/search">
<table border="0" cellpadding="3" cellspacing="0">
<tr><td align="right"><font face="Arial" size="2">%s</font></td>
<td><input type="text" name="memberno" size="12" maxlength="10" value="%s"></td>
<td><input type="button" value="%s" onclick="this.form.submit()"></td></tr>
</table></form>`, esc(v.MemberNoLabel), esc(value), esc(v.SearchLabel)))
}

func (s *Server) inquiry(w http.ResponseWriter, r *http.Request, sess *session) {
	s.inquiryForm(w, s.cfg.Variant.InquiryNav, "", "")
}

var digits = regexp.MustCompile(`^\d+$`)

func (s *Server) search(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/inquiry", http.StatusSeeOther)
		return
	}
	no := strings.TrimSpace(r.FormValue("memberno"))
	title := s.cfg.Variant.InquiryNav
	if !digits.MatchString(no) {
		s.inquiryForm(w, title, `<font color="#CC0000"><b>Validation Error: Member number must be numeric.</b></font><br><br>`, no)
		return
	}
	s.mu.Lock()
	m := s.members[no]
	s.mu.Unlock()
	if m == nil {
		s.inquiryForm(w, title, fmt.Sprintf(`<font color="#CC0000"><b>No member found matching number %s.</b></font><br><br>`, esc(no)), no)
		return
	}
	http.Redirect(w, r, "/member?no="+url.QueryEscape(no), http.StatusSeeOther)
}

func (s *Server) lookup(w http.ResponseWriter, sess *session, no string) *member {
	s.mu.Lock()
	m := s.members[no]
	s.mu.Unlock()
	if m == nil {
		s.inquiryForm(w, s.cfg.Variant.InquiryNav, fmt.Sprintf(`<font color="#CC0000"><b>No member found matching number %s.</b></font><br><br>`, esc(no)), no)
		return nil
	}
	if m.Restricted {
		s.page(w, http.StatusOK, "Access Denied", fmt.Sprintf(
			`<font color="#CC0000"><b>Access denied (SEC-403).</b></font><br><br>Operator %s is not authorized to view employee accounts.`, esc(sess.Operator)))
		return nil
	}
	return m
}

func (s *Server) memberDetail(w http.ResponseWriter, r *http.Request, sess *session) {
	m := s.lookup(w, sess, r.URL.Query().Get("no"))
	if m == nil {
		return
	}
	s.mu.Lock()
	var rows strings.Builder
	for _, a := range m.Accounts {
		fmt.Fprintf(&rows, `<tr bgcolor="#FFFFFF"><td>%s</td><td>%s</td><td align="right">%s</td><td align="right">%s</td><td>%s</td></tr>`,
			esc(a.Suffix), esc(a.Description), money(a.Current), money(a.Available), esc(a.Status))
	}
	s.mu.Unlock()
	field := func(label, value string) string {
		return fmt.Sprintf(`<tr><td align="right" bgcolor="#D4D0C8"><font face="Arial" size="2">%s</font></td><td bgcolor="#FFFFFF"><font face="Arial" size="2">%s</font></td></tr>`, label, esc(value))
	}
	s.page(w, http.StatusOK, "Member Detail", fmt.Sprintf(`
<table border="0" cellpadding="0" cellspacing="0"><tr><td valign="top">
<table border="0" cellpadding="3" cellspacing="1" bgcolor="#808080">
%s%s%s%s%s%s%s
</table>
</td></tr><tr><td><br><font face="Arial" size="2"><b>Share Accounts</b></font><br>
<table border="0" cellpadding="4" cellspacing="1" bgcolor="#808080">
<tr bgcolor="#C0C0C0"><td><b>Suffix</b></td><td><b>Description</b></td><td><b>Current Balance</b></td><td><b>Available Balance</b></td><td><b>Status</b></td></tr>
%s
</table>
</td></tr><tr><td><br>
<a href="/subaccount/new?no=%s">Open Sub-Account</a> &nbsp;|&nbsp; <a href="/inquiry">New Inquiry</a>
</td></tr></table>`,
		field("Member Name:", m.Name), field("Member Number:", m.Number), field("SSN/TIN:", m.SSN),
		field("Date of Birth:", m.DOB), field("Address:", m.Address), field("Phone:", m.Phone), field("Status:", m.Status),
		rows.String(), url.QueryEscape(m.Number)))
}

// ---- open sub-account: form -> (override) -> review -> confirm ----

func (s *Server) subForm(w http.ResponseWriter, m *member, notice string, app application, depositRaw string) {
	var opts strings.Builder
	opts.WriteString(`<option value="">-- Select --</option>`)
	for _, t := range accountTypes {
		sel := ""
		if t.Name == app.Type {
			sel = " selected"
		}
		fmt.Fprintf(&opts, `<option value="%s"%s>%s</option>`, esc(t.Name), sel, esc(t.Name))
	}
	s.page(w, http.StatusOK, "Open Sub-Account", notice+fmt.Sprintf(`
Member %s &mdash; %s<br><br>
<form method="post" action="/subaccount/review"><input type="hidden" name="no" value="%s">
<table border="0" cellpadding="3" cellspacing="0">
<tr><td align="right"><font face="Arial" size="2">Account Type:</font></td><td><select name="type">%s</select></td></tr>
<tr><td align="right"><font face="Arial" size="2">Nickname:</font></td><td><input type="text" name="nickname" size="24" maxlength="30" value="%s"></td></tr>
<tr><td align="right"><font face="Arial" size="2">Opening Deposit:</font></td><td><input type="text" name="deposit" size="12" value="%s"></td></tr>
<tr><td></td><td><input type="submit" value="Continue"></td></tr>
</table></form>
<a href="/member?no=%s">Cancel</a>`,
		esc(m.Number), esc(m.Name), esc(m.Number), opts.String(), esc(app.Nickname), esc(depositRaw), url.QueryEscape(m.Number)))
}

func (s *Server) subNew(w http.ResponseWriter, r *http.Request, sess *session) {
	m := s.lookup(w, sess, r.URL.Query().Get("no"))
	if m == nil {
		return
	}
	s.subForm(w, m, "", application{}, "")
}

func (s *Server) subReview(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		s.mu.Lock()
		var m *member
		var app application
		pending := sess.Pending != nil
		if pending {
			app = *sess.Pending
			m = s.members[app.Member]
		}
		s.mu.Unlock()
		switch {
		case !pending:
			http.Redirect(w, r, "/inquiry", http.StatusSeeOther)
		case app.Deposit >= overrideThreshold && !app.Overridden:
			http.Redirect(w, r, "/subaccount/override", http.StatusSeeOther)
		default:
			s.reviewPage(w, m, app)
		}
		return
	}
	m := s.lookup(w, sess, r.FormValue("no"))
	if m == nil {
		return
	}
	app := application{Member: m.Number, Type: r.FormValue("type"), Nickname: strings.TrimSpace(r.FormValue("nickname"))}
	raw := strings.TrimSpace(r.FormValue("deposit"))
	fail := func(msg string) {
		s.subForm(w, m, fmt.Sprintf(`<font color="#CC0000"><b>Validation Error: %s</b></font><br><br>`, esc(msg)), app, raw)
	}
	var min int64 = -1
	for _, t := range accountTypes {
		if t.Name == app.Type {
			min = t.Min
		}
	}
	if min < 0 {
		fail("Account type is required.")
		return
	}
	dep, ok := parseMoney(raw)
	if !ok {
		fail("Opening deposit must be a dollar amount.")
		return
	}
	if dep < min {
		fail(fmt.Sprintf("Opening deposit is below the %s minimum for %s.", money(min), app.Type))
		return
	}
	app.Deposit = dep
	s.mu.Lock()
	sess.Pending = &app
	s.mu.Unlock()
	http.Redirect(w, r, "/subaccount/review", http.StatusSeeOther)
}

func (s *Server) overridePage(w http.ResponseWriter, notice string) {
	s.page(w, http.StatusOK, "Supervisor Override Required", notice+fmt.Sprintf(`
Opening deposits of %s or more require a supervisor to countersign.<br><br>
<form method="post" action="/subaccount/override">
<table border="0" cellpadding="3" cellspacing="0">
<tr><td align="right"><font face="Arial" size="2">Override Code:</font></td><td><input type="password" name="code" size="12"></td>
<td><input type="submit" value="Authorize"></td></tr>
</table></form>`, money(overrideThreshold)))
}

func (s *Server) subOverride(w http.ResponseWriter, r *http.Request, sess *session) {
	s.mu.Lock()
	app := sess.Pending
	s.mu.Unlock()
	if app == nil {
		http.Redirect(w, r, "/inquiry", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodPost {
		s.overridePage(w, "")
		return
	}
	if r.FormValue("code") != s.cfg.OverrideCode {
		s.overridePage(w, `<font color="#CC0000"><b>Override code rejected.</b></font><br><br>`)
		return
	}
	s.mu.Lock()
	app.Overridden = true
	s.mu.Unlock()
	http.Redirect(w, r, "/subaccount/review", http.StatusSeeOther)
}

func (s *Server) reviewPage(w http.ResponseWriter, m *member, app application) {
	row := func(label, value string) string {
		return fmt.Sprintf(`<tr><td align="right" bgcolor="#D4D0C8">%s</td><td bgcolor="#FFFFFF">%s</td></tr>`, label, esc(value))
	}
	s.page(w, http.StatusOK, "Review New Sub-Account", fmt.Sprintf(`
Verify the details below. Confirming opens the account and posts the opening deposit.<br><br>
<table border="0" cellpadding="4" cellspacing="1" bgcolor="#808080">%s%s%s%s</table><br>
<form method="post" action="/subaccount/confirm">
<input type="submit" value="Confirm" onclick="return confirm('Open this sub-account and post the opening deposit?')"> &nbsp; <input type="button" value="Cancel" onclick="location.href='/member?no=%s'">
</form>`,
		row("Member Number:", m.Number), row("Account Type:", app.Type), row("Nickname:", app.Nickname),
		row("Opening Deposit:", money(app.Deposit)), url.QueryEscape(m.Number)))
}

func (s *Server) subConfirm(w http.ResponseWriter, r *http.Request, sess *session) {
	s.mu.Lock()
	app := sess.Pending
	if r.Method != http.MethodPost || app == nil || (app.Deposit >= overrideThreshold && !app.Overridden) {
		s.mu.Unlock()
		http.Redirect(w, r, "/inquiry", http.StatusSeeOther)
		return
	}
	sess.Pending = nil
	m := s.members[app.Member]
	last := 0
	for _, a := range m.Accounts {
		if n, _ := strconv.Atoi(a.Suffix); n > last {
			last = n
		}
	}
	suffix := fmt.Sprintf("%04d", last+1)
	m.Accounts = append(m.Accounts, account{suffix, app.Type, app.Deposit, app.Deposit, "Open"})
	s.confirms++
	conf := fmt.Sprintf("MC-%06d", 481200+s.confirms)
	cp := *app
	s.mu.Unlock()

	row := func(label, value string) string {
		return fmt.Sprintf(`<tr><td align="right" bgcolor="#D4D0C8">%s</td><td bgcolor="#FFFFFF">%s</td></tr>`, label, esc(value))
	}
	s.page(w, http.StatusOK, "Sub-Account Opened", fmt.Sprintf(`
<font color="#006600"><b>The sub-account has been opened.</b></font><br><br>
<table border="0" cellpadding="4" cellspacing="1" bgcolor="#808080">%s%s%s%s%s</table><br>
<a href="/member?no=%s">Return to Member Detail</a>`,
		row("Member Number:", m.Number), row("New Suffix:", suffix), row("Account Type:", cp.Type),
		row("Opening Deposit:", money(cp.Deposit)), row("Confirmation Number:", conf), url.QueryEscape(m.Number)))
}

// ---- simulator control plane (never reachable by the automation) ----

func (s *Server) simFault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST kind=notice|slow|error|expire [after=N] [count=N] [delay_ms=N]", http.StatusMethodNotAllowed)
		return
	}
	atoi := func(k string, def int) int {
		if n, err := strconv.Atoi(r.FormValue(k)); err == nil {
			return n
		}
		return def
	}
	f := &Fault{Kind: r.FormValue("kind"), After: atoi("after", 0), Count: atoi("count", 1), DelayMS: atoi("delay_ms", 3000)}
	switch f.Kind {
	case "notice", "slow", "error", "expire":
	default:
		http.Error(w, "unknown fault kind", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.faults = append(s.faults, f)
	s.mu.Unlock()
	json.NewEncoder(w).Encode(f)
}

func (s *Server) simReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.reset()
	fmt.Fprintln(w, "reset")
}

func (s *Server) simState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{
		"variant": s.cfg.Variant.Name, "sessions": len(s.sessions), "faults": s.faults, "confirmations": s.confirms,
	})
}
