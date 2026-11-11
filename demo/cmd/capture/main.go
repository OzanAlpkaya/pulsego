// Command capture records the PulseGo dashboard with headless Chromium: three
// stills and a sequence of numbered frames for the demo GIF.
//
// It signs in with the API key from the PULSEGO_DEMO_API_KEY environment
// variable (never a flag, so it does not show up in the process list), waits
// until every monitor has uptime and p95 data, saves dashboard.png, then
// drives the incident story through the demo targets' control endpoint:
// flaky goes down until the dashboard shows an open incident
// (incident-open.png), comes back up until that incident is resolved
// (incident-resolved.png). Frames are written throughout.
//
// The dashboard refreshes itself every 10 s; capture only observes it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/chromedp"
)

// Page conditions, evaluated in the dashboard. They read the rendered DOM only.
const (
	// Every monitor row has all its cells filled (no "—" placeholders), and
	// there are at least %d rows.
	jsDataReady = `(() => {
		const rows = document.querySelectorAll('#monitors-body tr');
		return rows.length >= %d && !document.querySelector('#monitors-body td.na');
	})()`
	// The named monitor shows the given state badge.
	jsMonitorState = `(() => {
		for (const tr of document.querySelectorAll('#monitors-body tr')) {
			const name = tr.querySelector('.monitor-name');
			if (name && name.textContent === %q) {
				return !!tr.querySelector('.badge-' + %q);
			}
		}
		return false;
	})()`
	// The incidents table has an open (or, with resolved, a resolved and no
	// open) incident for the named monitor.
	jsIncident = `(() => {
		let open = false, resolved = false;
		for (const tr of document.querySelectorAll('#incidents-body tr')) {
			const name = tr.querySelector('.monitor-name');
			if (!name || name.textContent !== %q) continue;
			if (tr.querySelector('.badge-down')) open = true;
			if (tr.querySelector('.badge-up')) resolved = true;
		}
		return %t ? (resolved && !open) : open;
	})()`
)

type capturer struct {
	ctx    context.Context
	out    string
	frames string
	every  time.Duration
	n      int
}

func main() {
	dashURL := flag.String("url", "http://127.0.0.1:18080/", "dashboard URL (a local address)")
	control := flag.String("flaky-control", "http://127.0.0.1:18082/control/flaky", "control endpoint of the demo targets")
	flakyName := flag.String("flaky-name", "Payments webhook", "name of the monitor that watches /flaky")
	monitors := flag.Int("monitors", 4, "number of monitors to wait for")
	out := flag.String("out", "docs/screenshots", "directory for the PNG stills")
	frames := flag.String("frames", "", "directory for numbered GIF frames (required)")
	every := flag.Duration("every", 2*time.Second, "time between GIF frames")
	hold := flag.Int("hold", 3, "extra frames kept after each milestone")
	width := flag.Int64("width", 1280, "viewport width")
	height := flag.Int64("height", 760, "viewport height")
	timeout := flag.Duration("timeout", 6*time.Minute, "deadline for the whole capture")
	flag.Parse()

	key := os.Getenv("PULSEGO_DEMO_API_KEY")
	if key == "" {
		log.Fatal("PULSEGO_DEMO_API_KEY is not set")
	}
	if *frames == "" {
		log.Fatal("-frames is required")
	}
	for _, dir := range []string{*out, *frames} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			log.Fatal(err)
		}
	}

	// The deadline bounds the whole browser: it stops when the context ends.
	root, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("hide-scrollbars", true),
	)
	actx, cancel := chromedp.NewExecAllocator(root, opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()

	c := &capturer{ctx: ctx, out: *out, frames: *frames, every: *every}
	if err := c.run(*dashURL, key, *control, *flakyName, *monitors, *hold, *width, *height); err != nil {
		log.Fatalf("capture: %v", err)
	}
	fmt.Printf("wrote 3 stills to %s and %d frames to %s\n", *out, c.n, *frames)
}

func (c *capturer) run(dashURL, key, control, flaky string, monitors, hold int, w, h int64) error {
	// Make sure the story starts from a healthy flaky target.
	if err := setFlaky(c.ctx, control, "up"); err != nil {
		return err
	}

	if err := chromedp.Do(c.ctx,
		chromedp.EmulateViewport(w, h),
		chromedp.Navigate(dashURL),
		chromedp.WaitVisible(chromedp.CSS("#key-input")),
		chromedp.SendKeys(chromedp.CSS("#key-input"), key),
		chromedp.Click(chromedp.CSS("#key-form button[type=submit]")),
		chromedp.WaitVisible(chromedp.CSS("#monitors-table")),
	); err != nil {
		return fmt.Errorf("open dashboard: %w", err)
	}

	log.Print("waiting for uptime and p95 on every monitor")
	if err := c.until(fmt.Sprintf(jsDataReady, monitors), false); err != nil {
		return fmt.Errorf("wait for data: %w", err)
	}
	if err := c.until(fmt.Sprintf(jsMonitorState, flaky, "up"), false); err != nil {
		return fmt.Errorf("wait for %s up: %w", flaky, err)
	}
	if err := c.still("dashboard.png"); err != nil {
		return err
	}
	if err := c.holdFrames(hold); err != nil {
		return err
	}

	log.Printf("switching %s down", flaky)
	if err := setFlaky(c.ctx, control, "down"); err != nil {
		return err
	}
	if err := c.until(fmt.Sprintf(jsIncident, flaky, false), true); err != nil {
		return fmt.Errorf("wait for open incident: %w", err)
	}
	if err := c.still("incident-open.png"); err != nil {
		return err
	}
	if err := c.holdFrames(hold); err != nil {
		return err
	}

	log.Printf("switching %s up", flaky)
	if err := setFlaky(c.ctx, control, "up"); err != nil {
		return err
	}
	if err := c.until(fmt.Sprintf(jsIncident, flaky, true), true); err != nil {
		return fmt.Errorf("wait for resolved incident: %w", err)
	}
	if err := c.still("incident-resolved.png"); err != nil {
		return err
	}
	return c.holdFrames(hold + 2)
}

// until polls the page condition every c.every, recording a frame per poll
// when frames is true, until the condition holds. It also records a frame
// for the moment the condition first holds.
func (c *capturer) until(cond string, frames bool) error {
	for {
		ok, err := chromedp.Run(c.ctx, chromedp.Evaluate[bool](cond))
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if frames {
			if err := c.frame(); err != nil {
				return err
			}
		}
		if err := chromedp.Do(c.ctx, chromedp.Sleep(c.every)); err != nil {
			return err
		}
	}
}

func (c *capturer) holdFrames(n int) error {
	for i := 0; i < n; i++ {
		if i > 0 {
			if err := chromedp.Do(c.ctx, chromedp.Sleep(c.every)); err != nil {
				return err
			}
		}
		if err := c.frame(); err != nil {
			return err
		}
	}
	return nil
}

func (c *capturer) frame() error {
	png, err := chromedp.Run(c.ctx, chromedp.CaptureScreenshot())
	if err != nil {
		return fmt.Errorf("frame %d: %w", c.n+1, err)
	}
	c.n++
	return writeNonEmpty(filepath.Join(c.frames, fmt.Sprintf("%03d.png", c.n)), png)
}

func (c *capturer) still(name string) error {
	png, err := chromedp.Run(c.ctx, chromedp.CaptureScreenshot())
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	log.Printf("captured %s", name)
	if err := writeNonEmpty(filepath.Join(c.out, name), png); err != nil {
		return err
	}
	// The still is also a GIF frame, so the animation shows every milestone.
	c.n++
	return writeNonEmpty(filepath.Join(c.frames, fmt.Sprintf("%03d.png", c.n)), png)
}

func writeNonEmpty(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New(path + ": browser returned an empty image")
	}
	return os.WriteFile(path, data, 0o600)
}

func setFlaky(ctx context.Context, control, state string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, control+"?state="+state, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("set flaky %s: %w", state, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("set flaky %s: control endpoint answered %s", state, res.Status)
	}
	return nil
}
