// Command consolerec records the operator console being used during a
// handoff: it drives the console page in a browser the way a person would
// and saves a frame every few hundred milliseconds. Not part of the product.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

func main() {
	out := os.Args[1]
	os.MkdirAll(out, 0o755)

	replay := exec.Command("./bin/cua", "replay", "--artifact", "artifacts/member.open_sub_account.json",
		"--tenant", "tenants/pinecrest.json", "--operator", "127.0.0.1:8090", "--quiet",
		"--input", "member_id=10058", "--input", "account_type=Money Market", "--input", "nickname=Reserve",
		"--input", "opening_deposit=12000.00", "--run-dir", filepath.Join(out, "run"))
	replay.Stdout, _ = os.Create(filepath.Join(out, "result.json"))
	replay.Stderr = os.Stderr
	if err := replay.Start(); err != nil {
		log.Fatal(err)
	}

	actx, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("force-device-scale-factor", "1"), chromedp.WindowSize(1500, 950))...)
	defer cancel()
	ctx, cancel2 := chromedp.NewContext(actx)
	defer cancel2()

	var frame atomic.Int64
	// pause waits while saving a frame every 250 ms, so the recording shows
	// the console updating between actions.
	pause := func(d time.Duration) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			end := time.Now().Add(d)
			for time.Now().Before(end) {
				var png []byte
				if err := chromedp.CaptureScreenshot(&png).Do(ctx); err == nil {
					os.WriteFile(filepath.Join(out, fmt.Sprintf("f%05d.png", frame.Add(1))), png, 0o644)
				}
				time.Sleep(250 * time.Millisecond)
			}
			return nil
		})
	}

	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	// The live view is 1280x900 shown at the <img> width; map app coordinates.
	imgClick := func(ax, ay float64) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			var box []float64 // x, y, w, h of the img on the page, then its natural size
			if err := chromedp.Evaluate(`(() => { const i = document.getElementById('screen'), r = i.getBoundingClientRect(); return [r.left, r.top, r.width, r.height, i.naturalWidth, i.naturalHeight]; })()`, &box).Do(ctx); err != nil {
				return err
			}
			x := box[0] + ax*box[2]/box[4]
			y := box[1] + ay*box[3]/box[5]
			if err := input.DispatchMouseEvent(input.MousePressed, x, y).WithButton(input.Left).WithClickCount(1).Do(ctx); err != nil {
				return err
			}
			return input.DispatchMouseEvent(input.MouseReleased, x, y).WithButton(input.Left).WithClickCount(1).Do(ctx)
		})
	}
	click := func(text string) chromedp.Action {
		return chromedp.Click(fmt.Sprintf(`//button[normalize-space(text())=%q]`, text), chromedp.BySearch)
	}

	time.Sleep(4 * time.Second) // let the run reach the override screen
	must(chromedp.Run(ctx,
		chromedp.Navigate("http://127.0.0.1:8090/"),
		pause(3*time.Second),
		chromedp.Clear("#operator"), chromedp.SendKeys("#operator", "sam.supervisor"),
		pause(2*time.Second),
		click("Take control"), pause(3*time.Second),
		imgClick(340, 142), pause(2*time.Second), // the Override Code field
		chromedp.SendKeys("#text", "SUP-4471"), pause(1500*time.Millisecond),
		click("Type"), pause(2500*time.Millisecond),
		imgClick(431, 142), pause(3*time.Second), // Authorize
		chromedp.SendKeys("#note", "countersigned the large opening deposit"), pause(1500*time.Millisecond),
		click("Hand back: resume where you paused"), pause(5*time.Second), // automation continues, then asks approval
		chromedp.SendKeys("#note", "review screen matches the request"), pause(1500*time.Millisecond),
		click("Approve"), pause(6*time.Second),
	))
	must(replay.Wait())
	fmt.Println("frames:", frame.Load())
}
