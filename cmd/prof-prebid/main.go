package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// version is the semantic version of the prof-prebid CLI.
// Bump following semver (MAJOR.MINOR.PATCH) on every release.
const version = "0.1.0"

const safariUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_7_4) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15"

// extractScript runs in the browser to find all pbjs instances and return their s2sConfig.
const extractScript = `
(function() {
	// Detect known publisher wrapper / ad management platforms.
	function detectPublisher() {
		if (window.freestar && window.freestar.config) return 'Freestar';
		if (window.freestar)                            return 'Freestar';
		if (window.Browsi)                              return 'Browsi';
		if (window.AdThrive)                            return 'AdThrive';
		if (window.Mediavine)                           return 'Mediavine';
		if (window.rp_account || window.rp)            return 'Raptive';
		if (window.ntvConfig || window.ntv)             return 'Nativo';
		return null;
	}

	// Prebid instances can live under different namespaces (default: pbjs).
	var namespaces = ['pbjs'];
	for (var key in window) {
		try {
			if (key !== 'pbjs' && window[key] && typeof window[key].getConfig === 'function' && typeof window[key].version === 'string') {
				namespaces.push(key);
			}
		} catch(e) {}
	}

	var publisher = detectPublisher();
	var results = [];
	namespaces.forEach(function(ns) {
		var pbjs = window[ns];
		if (!pbjs || typeof pbjs.getConfig !== 'function') return;
		try {
			var cfg = pbjs.getConfig();
			var s2s = cfg.s2sConfig;
			if (!s2s) return;
			results.push({
				namespace: ns,
				version: pbjs.version || 'unknown',
				publisher: publisher,
				s2sConfig: s2s,
			});
		} catch(e) {}
	});
	return JSON.stringify(results);
})()
`

type S2SEndpoint struct {
	P1 string `json:"p1,omitempty"`
	P2 string `json:"p2,omitempty"`
	// catch-all for arbitrary key→url maps
}

type S2SResult struct {
	Namespace string  `json:"namespace"`
	Version   string  `json:"version"`
	Publisher *string `json:"publisher"`
	S2SConfig any     `json:"s2sConfig"`
}

// extractEndpoint pulls the Prebid Server URL from s2sConfig.
// The endpoint field can be a plain string or a map of bidder→url.
func extractEndpoint(s2s any) string {
	m, ok := s2s.(map[string]any)
	if !ok {
		return "(not set)"
	}
	ep, ok := m["endpoint"]
	if !ok {
		return "(not set)"
	}
	switch v := ep.(type) {
	case string:
		return v
	case map[string]any:
		// Return all entries as "key: url" lines
		out := ""
		for k, u := range v {
			out += fmt.Sprintf("\n  %s: %v", k, u)
		}
		return out
	default:
		b, _ := json.Marshal(ep)
		return string(b)
	}
}

// fetchResults launches a browser (headed or not) and returns the extracted s2sConfig results.
func fetchResults(url string, headed bool, wait, timeout time.Duration) ([]S2SResult, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent(safariUA),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if headed {
		opts = append(opts, chromedp.Flag("headless", false))
	} else {
		opts = append(opts, chromedp.Flag("headless", "new"))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancelCtx()

	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	antiDetectAction := chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(`
			Object.defineProperty(navigator, 'webdriver',    {get: () => undefined});
			Object.defineProperty(navigator, 'plugins',      {get: () => [1,2,3,4,5]});
			Object.defineProperty(navigator, 'languages',    {get: () => ['en-US','en']});
			Object.defineProperty(screen,    'width',        {get: () => 1920});
			Object.defineProperty(screen,    'height',       {get: () => 1080});
			Object.defineProperty(screen,    'availWidth',   {get: () => 1920});
			Object.defineProperty(screen,    'availHeight',  {get: () => 1040});
			Object.defineProperty(window,    'outerWidth',   {get: () => 1920});
			Object.defineProperty(window,    'outerHeight',  {get: () => 1040});
			Object.defineProperty(window,    'innerWidth',   {get: () => 1920});
			Object.defineProperty(window,    'innerHeight',  {get: () => 1040});
		`).Do(ctx)
		return err
	})

	setViewport := chromedp.ActionFunc(func(ctx context.Context) error {
		return emulation.SetDeviceMetricsOverride(1920, 1080, 1.0, false).Do(ctx)
	})

	var raw string
	if err := chromedp.Run(ctx,
		antiDetectAction,
		chromedp.Navigate(url),
		setViewport,
		chromedp.Sleep(wait),
		chromedp.Evaluate(extractScript, &raw),
	); err != nil {
		return nil, err
	}

	var results []S2SResult
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		return nil, fmt.Errorf("failed to parse result: %w (raw: %s)", err, raw)
	}
	return results, nil
}

func main() {
	timeout := flag.Duration("timeout", 45*time.Second, "Page load timeout")
	outputJSON := flag.Bool("json", false, "Output raw JSON")
	wait := flag.Duration("wait", 10*time.Second, "Time to wait after page load for ad scripts to initialise")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: prof-prebid [flags] <url>\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("prof-prebid v%s\n", version)
		return
	}

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	url := flag.Arg(0)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}

	// First attempt: headless (fast, no visible window).
	results, err := fetchResults(url, false, *wait, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "headless error: %v\n", err)
		os.Exit(1)
	}

	// If nothing found, retry with a visible window to bypass stricter bot detection.
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "headless: no results, retrying with headed browser...")
		results, err = fetchResults(url, true, *wait, *timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "headed error: %v\n", err)
			os.Exit(1)
		}
	}

	if len(results) == 0 {
		fmt.Println("No Prebid s2sConfig found on this page.")
		return
	}

	if *outputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
		return
	}

	for _, r := range results {
		fmt.Printf("=== Prebid instance: %s (v%s) ===\n", r.Namespace, r.Version)
		if r.Publisher != nil {
			fmt.Printf("Publisher wrapper: %s\n", *r.Publisher)
		}
		fmt.Printf("Prebid Server URL: %s\n", extractEndpoint(r.S2SConfig))
		pretty, _ := json.MarshalIndent(r.S2SConfig, "", "  ")
		fmt.Printf("s2sConfig:\n%s\n\n", pretty)
	}
}
