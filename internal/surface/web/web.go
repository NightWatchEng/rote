// Package web is the browser adapter for surface.Surface.
//
// It perceives through the accessibility tree (one per frame, fetched over
// the Chrome DevTools Protocol) and acts through synthesized mouse and
// keyboard input at screen coordinates. It never queries the DOM by selector:
// framesets, layout tables and missing ids do not matter to it, and the same
// perceive-by-role-and-rectangle / act-by-coordinate shape is what a desktop
// accessibility API offers.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"

	"github.com/NightWatchEng/rote/internal/surface"
)

// Gate decides whether the browser may make a request. Returning an error
// fails the request at the network layer, before it leaves the machine.
type Gate func(method, url string) error

// Options configure a browser session.
type Options struct {
	Headless bool
	Gate     Gate
}

const (
	viewportW = 1280
	viewportH = 900
	// callTimeout bounds any single protocol call so a hung page surfaces as
	// an error instead of a stuck run.
	callTimeout = 15 * time.Second
)

type nativeDialog struct {
	kind    string // alert | confirm | prompt | beforeunload
	message string
}

// Web is one live browser session.
type Web struct {
	ctx    context.Context
	cancel []context.CancelFunc
	gate   Gate

	mu       sync.Mutex
	seq      int
	nodes    map[int]int64 // element ref -> backend node id, for the latest observation
	dialog   *nativeDialog
	dialogUp chan struct{} // receives when a native dialog opens
	lastShot []byte
}

// Refs used for the synthetic elements of a native dialog.
const (
	dialogAccept  = -1
	dialogDismiss = -2
)

// New launches a browser.
func New(opts Options) (*Web, error) {
	flags := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", opts.Headless),
		chromedp.Flag("force-device-scale-factor", "1"),
		chromedp.Flag("hide-scrollbars", false),
		chromedp.WindowSize(viewportW, viewportH),
	)
	actx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), flags...)
	ctx, cancelCtx := chromedp.NewContext(actx)
	w := &Web{ctx: ctx, cancel: []context.CancelFunc{cancelCtx, cancelAlloc}, gate: opts.Gate, dialogUp: make(chan struct{}, 1)}

	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *fetch.EventRequestPaused:
			go w.onRequest(e)
		case *page.EventJavascriptDialogOpening:
			w.mu.Lock()
			w.dialog = &nativeDialog{kind: string(e.Type), message: e.Message}
			w.mu.Unlock()
			select {
			case w.dialogUp <- struct{}{}:
			default:
			}
		case *page.EventJavascriptDialogClosed:
			w.mu.Lock()
			w.dialog = nil
			w.mu.Unlock()
		}
	})

	start := []chromedp.Action{page.Enable()}
	if opts.Gate != nil {
		start = append(start, fetch.Enable())
	}
	if err := chromedp.Run(ctx, start...); err != nil {
		w.Close()
		return nil, fmt.Errorf("starting browser (is Chrome or Chromium installed?): %w", err)
	}
	return w, nil
}

func (w *Web) Close() error {
	for _, c := range w.cancel {
		c()
	}
	return nil
}

// onRequest applies the gate to every request the page makes — top-level
// navigations, frame loads and form posts alike.
func (w *Web) onRequest(e *fetch.EventRequestPaused) {
	ctx := cdp.WithExecutor(w.ctx, chromedp.FromContext(w.ctx).Target)
	if err := w.gate(e.Request.Method, e.Request.URL); err != nil {
		fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
		return
	}
	fetch.ContinueRequest(e.RequestID).Do(ctx)
}

// do runs protocol calls on the browser, bounded by both the caller's
// context and callTimeout.
func (w *Web) do(ctx context.Context, fn func(ctx context.Context) error) error {
	cctx, cancel := context.WithTimeout(w.ctx, callTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return chromedp.Run(cctx, chromedp.ActionFunc(fn))
}

func (w *Web) openDialog() *nativeDialog {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dialog
}

// Open starts a fresh application session: cookies are cleared first, so a
// flow restarted after a failure begins signed out, exactly as it was
// recorded, instead of inheriting whatever state the last attempt left.
func (w *Web) Open(ctx context.Context, url string) error {
	return w.do(ctx, func(ctx context.Context) error {
		if err := network.ClearBrowserCookies().Do(ctx); err != nil {
			return err
		}
		return chromedp.Navigate(url).Do(ctx)
	})
}

// ---- perception ----

type axValue struct {
	Value json.RawMessage `json:"value"`
}

func (v *axValue) str() string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v.Value, &s) == nil {
		return s
	}
	return strings.Trim(string(v.Value), `"`)
}

type axNode struct {
	NodeID     string   `json:"nodeId"`
	Ignored    bool     `json:"ignored"`
	Role       *axValue `json:"role"`
	Name       *axValue `json:"name"`
	Value      *axValue `json:"value"`
	ParentID   string   `json:"parentId"`
	ChildIDs   []string `json:"childIds"`
	BackendID  int64    `json:"backendDOMNodeId"`
	Properties []struct {
		Name  string  `json:"name"`
		Value axValue `json:"value"`
	} `json:"properties"`
}

func (n *axNode) prop(name string) string {
	for _, p := range n.Properties {
		if p.Name == name {
			return p.Value.str()
		}
	}
	return ""
}

var controlRoles = map[string]string{
	"textbox":    surface.RoleTextbox,
	"searchbox":  surface.RoleTextbox,
	"spinbutton": surface.RoleTextbox,
	"button":     surface.RoleButton,
	"link":       surface.RoleLink,
	"combobox":   surface.RoleSelect,
	"listbox":    surface.RoleSelect,
	"checkbox":   surface.RoleCheckbox,
	"radio":      surface.RoleRadio,
}

var cellRoles = map[string]bool{
	"cell": true, "gridcell": true, "columnheader": true, "rowheader": true, "LayoutTableCell": true,
}

// Observe snapshots every frame. While a native dialog is open it is the
// only thing reported: the page behind it is frozen and unreachable, exactly
// as it is for a person.
func (w *Web) Observe(ctx context.Context) (*surface.Observation, error) {
	w.mu.Lock()
	w.seq++
	seq := w.seq
	d := w.dialog
	w.mu.Unlock()

	if d != nil {
		obs := &surface.Observation{Seq: seq, Frames: []surface.Frame{{Name: surface.DialogFrame, Title: d.kind}}}
		obs.Elements = []surface.Element{
			{Ref: 1, Frame: surface.DialogFrame, Role: surface.RoleText, Name: d.message},
			{Ref: 2, Frame: surface.DialogFrame, Role: surface.RoleButton, Name: "OK"},
		}
		nodes := map[int]int64{2: dialogAccept}
		if d.kind != "alert" {
			obs.Elements = append(obs.Elements, surface.Element{Ref: 3, Frame: surface.DialogFrame, Role: surface.RoleButton, Name: "Cancel"})
			nodes[3] = dialogDismiss
		}
		w.mu.Lock()
		w.nodes = nodes
		w.mu.Unlock()
		return obs, nil
	}

	obs := &surface.Observation{Seq: seq}
	nodes := map[int]int64{}
	err := w.do(ctx, func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		obs.URL = tree.Frame.URL
		return w.observeFrame(ctx, tree, "top", obs, nodes)
	})
	if err != nil {
		if w.openDialog() != nil { // a dialog opened mid-snapshot
			return w.Observe(ctx)
		}
		return nil, fmt.Errorf("observe: %w", err)
	}
	w.mu.Lock()
	w.nodes = nodes
	w.mu.Unlock()
	return obs, nil
}

func (w *Web) observeFrame(ctx context.Context, t *page.FrameTree, name string, obs *surface.Observation, nodes map[int]int64) error {
	var res struct {
		Nodes []axNode `json:"nodes"`
	}
	if err := cdp.Execute(ctx, "Accessibility.getFullAXTree", map[string]any{"frameId": t.Frame.ID}, &res); err != nil {
		return err
	}
	byID := make(map[string]*axNode, len(res.Nodes))
	var root *axNode
	for i := range res.Nodes {
		n := &res.Nodes[i]
		byID[n.NodeID] = n
		if n.ParentID == "" && root == nil {
			root = n
		}
	}
	frame := surface.Frame{Name: name, URL: t.Frame.URL}
	if root != nil {
		frame.Title = root.Name.str()
	}
	obs.Frames = append(obs.Frames, frame)

	add := func(el surface.Element, backend int64) {
		el.Ref = len(obs.Elements) + 1
		el.Frame = name
		obs.Elements = append(obs.Elements, el)
		nodes[el.Ref] = backend
	}

	// Depth-first over the tree gives document order.
	var walk func(n *axNode, cell *surface.Rect)
	walk = func(n *axNode, cell *surface.Rect) {
		role := n.Role.str()
		if role == "Iframe" || role == "IframePresentational" {
			return // child frames are observed on their own
		}
		if cellRoles[role] && !n.Ignored {
			if r, ok := bounds(ctx, n.BackendID); ok {
				cell = &r
			}
		}
		if !n.Ignored {
			if mapped, ok := controlRoles[role]; ok {
				if r, ok := bounds(ctx, n.BackendID); ok {
					el := surface.Element{Role: mapped, Name: n.Name.str(), Value: n.Value.str(), Bounds: r, Cell: cell}
					switch mapped {
					case surface.RoleLink:
						el.Href = n.prop("url")
					case surface.RoleSelect:
						el.Options = options(n, byID)
					case surface.RoleCheckbox, surface.RoleRadio:
						el.Value = n.prop("checked")
					case surface.RoleTextbox:
						if isPassword(ctx, n.BackendID) {
							el.Protected = true
							el.Value = strings.Repeat("•", utf8.RuneCountInString(el.Value))
						}
					}
					add(el, n.BackendID)
				}
				return // a control's inner text is part of the control
			}
			if role == "StaticText" {
				if text := strings.TrimSpace(n.Name.str()); text != "" {
					if r, ok := bounds(ctx, n.BackendID); ok {
						add(surface.Element{Role: surface.RoleText, Name: text, Bounds: r, Cell: cell}, n.BackendID)
					}
				}
				return
			}
			if role == "image" && n.Name.str() != "" {
				if r, ok := bounds(ctx, n.BackendID); ok {
					add(surface.Element{Role: surface.RoleImage, Name: n.Name.str(), Bounds: r, Cell: cell}, n.BackendID)
				}
				return
			}
		}
		for _, id := range n.ChildIDs {
			if c := byID[id]; c != nil {
				walk(c, cell)
			}
		}
	}
	if root != nil {
		walk(root, nil)
	}

	seen := map[string]int{}
	for i, child := range t.ChildFrames {
		childName := child.Frame.Name
		if childName == "" {
			childName = fmt.Sprintf("%s.%d", name, i+1)
		}
		if seen[childName]++; seen[childName] > 1 {
			childName = fmt.Sprintf("%s#%d", childName, seen[childName])
		}
		if err := w.observeFrame(ctx, child, childName, obs, nodes); err != nil {
			return err
		}
	}
	return nil
}

func options(n *axNode, byID map[string]*axNode) []string {
	var out []string
	var walk func(n *axNode)
	walk = func(n *axNode) {
		switch n.Role.str() {
		case "option", "menuitem", "MenuListOption", "listitem":
			if name := strings.TrimSpace(n.Name.str()); name != "" {
				out = append(out, name)
			}
			return
		}
		for _, id := range n.ChildIDs {
			if c := byID[id]; c != nil {
				walk(c)
			}
		}
	}
	for _, id := range n.ChildIDs {
		if c := byID[id]; c != nil {
			walk(c)
		}
	}
	return out
}

// bounds returns the on-screen rectangle of a node in top-level viewport
// coordinates, or false if it is not rendered.
func bounds(ctx context.Context, backend int64) (surface.Rect, bool) {
	if backend == 0 {
		return surface.Rect{}, false
	}
	var res struct {
		Quads [][]float64 `json:"quads"`
	}
	if err := cdp.Execute(ctx, "DOM.getContentQuads", map[string]any{"backendNodeId": backend}, &res); err != nil || len(res.Quads) == 0 {
		return surface.Rect{}, false
	}
	minX, minY, maxX, maxY := res.Quads[0][0], res.Quads[0][1], res.Quads[0][0], res.Quads[0][1]
	for _, q := range res.Quads {
		for i := 0; i+1 < len(q); i += 2 {
			minX, maxX = min(minX, q[i]), max(maxX, q[i])
			minY, maxY = min(minY, q[i+1]), max(maxY, q[i+1])
		}
	}
	r := surface.Rect{X: minX, Y: minY, W: maxX - minX, H: maxY - minY}
	return r, r.W > 0 && r.H > 0
}

func isPassword(ctx context.Context, backend int64) bool {
	var res struct {
		Node struct {
			Attributes []string `json:"attributes"`
		} `json:"node"`
	}
	if err := cdp.Execute(ctx, "DOM.describeNode", map[string]any{"backendNodeId": backend}, &res); err != nil {
		return false
	}
	for i := 0; i+1 < len(res.Node.Attributes); i += 2 {
		if strings.EqualFold(res.Node.Attributes[i], "type") && strings.EqualFold(res.Node.Attributes[i+1], "password") {
			return true
		}
	}
	return false
}

// ---- action ----

var errStale = errors.New("element is from an older observation; observe again before acting")

func (w *Web) backend(el surface.Element) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.nodes[el.Ref]
	if !ok {
		return 0, errStale
	}
	return id, nil
}

func (w *Web) Click(ctx context.Context, el surface.Element) error {
	backend, err := w.backend(el)
	if err != nil {
		return err
	}
	if backend == dialogAccept || backend == dialogDismiss {
		return w.answerDialog(ctx, backend == dialogAccept)
	}
	x, y, err := w.point(ctx, backend)
	if err != nil {
		return err
	}
	return w.clickAt(ctx, x, y, 1)
}

// point scrolls a node into view and returns its center.
func (w *Web) point(ctx context.Context, backend int64) (x, y float64, err error) {
	err = w.do(ctx, func(ctx context.Context) error {
		cdp.Execute(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": backend}, nil)
		r, ok := bounds(ctx, backend)
		if !ok {
			return errors.New("element is no longer rendered")
		}
		x, y = r.Center()
		return nil
	})
	return x, y, err
}

// clickAt presses and releases the left button. A click can open a native
// dialog, which freezes the page before the release is acknowledged; that is
// a completed click, not an error.
func (w *Web) clickAt(ctx context.Context, x, y float64, count int64) error {
	select {
	case <-w.dialogUp: // drain the signal of a dialog that has since closed
	default:
	}
	if w.openDialog() != nil {
		return errors.New("a native dialog is open; observe again and answer it first")
	}
	done := make(chan error, 1)
	go func() {
		done <- w.do(ctx, func(ctx context.Context) error {
			if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
				return err
			}
			if err := input.DispatchMouseEvent(input.MousePressed, x, y).WithButton(input.Left).WithClickCount(count).Do(ctx); err != nil {
				return err
			}
			return input.DispatchMouseEvent(input.MouseReleased, x, y).WithButton(input.Left).WithClickCount(count).Do(ctx)
		})
	}()
	select {
	case err := <-done:
		return err
	case <-w.dialogUp:
		return nil
	}
}

func (w *Web) answerDialog(ctx context.Context, accept bool) error {
	return w.do(ctx, func(ctx context.Context) error {
		return page.HandleJavaScriptDialog(accept).Do(ctx)
	})
}

// Type replaces the content of a text control: a triple-click selects what
// is there, then the text is inserted as keyboard input.
func (w *Web) Type(ctx context.Context, el surface.Element, text string) error {
	backend, err := w.backend(el)
	if err != nil {
		return err
	}
	x, y, err := w.point(ctx, backend)
	if err != nil {
		return err
	}
	if err := w.clickAt(ctx, x, y, 3); err != nil {
		return err
	}
	if text == "" {
		return w.Press(ctx, "Backspace")
	}
	return w.Keys(ctx, text)
}

// Select chooses an option of a drop-down by its visible label. Native
// drop-down popups are drawn outside the page and cannot be clicked through
// the protocol, so this one action sets the control directly and fires the
// events a user's choice would.
func (w *Web) Select(ctx context.Context, el surface.Element, option string) error {
	backend, err := w.backend(el)
	if err != nil {
		return err
	}
	return w.do(ctx, func(ctx context.Context) error {
		var node struct {
			Object struct {
				ObjectID string `json:"objectId"`
			} `json:"object"`
		}
		if err := cdp.Execute(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": backend}, &node); err != nil {
			return err
		}
		var out struct {
			Result struct {
				Value bool `json:"value"`
			} `json:"result"`
		}
		err := cdp.Execute(ctx, "Runtime.callFunctionOn", map[string]any{
			"objectId": node.Object.ObjectID,
			"functionDeclaration": `function(label) {
				const want = label.trim().toLowerCase();
				const opt = Array.from(this.options || []).find(o => o.text.trim().toLowerCase() === want);
				if (!opt) return false;
				this.value = opt.value;
				this.dispatchEvent(new Event('input', {bubbles: true}));
				this.dispatchEvent(new Event('change', {bubbles: true}));
				return true;
			}`,
			"arguments":   []map[string]any{{"value": option}},
			"returnValue": true,
		}, &out)
		if err != nil {
			return err
		}
		if !out.Result.Value {
			return fmt.Errorf("option %q is not offered by this control", option)
		}
		return nil
	})
}

func (w *Web) Pointer(ctx context.Context, x, y float64) error {
	if w.openDialog() != nil {
		return errors.New("a native dialog is open; press Enter to accept it or Escape to dismiss it")
	}
	return w.clickAt(ctx, x, y, 1)
}

func (w *Web) Keys(ctx context.Context, text string) error {
	return w.do(ctx, func(ctx context.Context) error {
		return input.InsertText(text).Do(ctx)
	})
}

var keys = map[string]string{"Enter": kb.Enter, "Tab": kb.Tab, "Backspace": kb.Backspace, "Escape": kb.Escape}

func (w *Web) Press(ctx context.Context, key string) error {
	if w.openDialog() != nil {
		switch key {
		case "Enter":
			return w.answerDialog(ctx, true)
		case "Escape":
			return w.answerDialog(ctx, false)
		}
	}
	code, ok := keys[key]
	if !ok {
		return fmt.Errorf("unsupported key %q", key)
	}
	return w.do(ctx, func(ctx context.Context) error {
		return chromedp.KeyEvent(code).Do(ctx)
	})
}

// Screenshot captures the viewport. While a native dialog is open the page
// cannot be painted, so the last frame before it is returned.
func (w *Web) Screenshot(ctx context.Context) ([]byte, error) {
	if w.openDialog() != nil {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.lastShot == nil {
			return nil, errors.New("a native dialog is open and no earlier frame is available")
		}
		return w.lastShot, nil
	}
	var shot []byte
	err := w.do(ctx, func(ctx context.Context) error {
		var err error
		shot, err = page.CaptureScreenshot().Do(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.lastShot = shot
	w.mu.Unlock()
	return shot, nil
}
