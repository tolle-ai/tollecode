package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// ---------------------------------------------------------------------------
// Shared process-level browser allocator
// ---------------------------------------------------------------------------
// chromedp allocates one Chrome *process* per ExecAllocator context. We create
// ONE ExecAllocator per Go process, then one chromedp context (== one browser
// tab) per agent session. Every tool for a session reuses the same tab, and
// each session's tab is fully isolated from every other session/sub-agent.
var (
	allocOnce   sync.Once
	allocCtx    context.Context // parent context — never cancelled until program exit
	allocCancel context.CancelFunc
)

func initAllocator() {
	allocOnce.Do(func() {
		opts := append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", false),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("disable-background-networking", true),
			chromedp.Flag("disable-background-timer-throttling", true),
			chromedp.Flag("disable-backgrounding-occluded-windows", true),
			chromedp.Flag("disable-renderer-backgrounding", true),
			chromedp.Flag("disable-features", "TranslateUI,BlinkGenPropertyTrees"),
			chromedp.Flag("disable-component-extensions-with-background-pages", true),
			chromedp.Flag("no-first-run", true),
			chromedp.Flag("disable-default-apps", true),
			chromedp.Flag("window-size", "1280,900"),
			// Always open the browser window fullscreen. window-size stays as the
			// fallback viewport; start-fullscreen makes the visible window fill the
			// screen (a follow-up CDP SetWindowBounds in ensureCtxLocked enforces it
			// even where the launch flag is ignored).
			chromedp.Flag("start-fullscreen", true),
			chromedp.Flag("disable-blink-features", "AutomationControlled"),
		)
		allocCtx, allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	})
}

// Tunable timeouts for browser operations.
const (
	navTimeout        = 35 * time.Second
	interactTimeout   = 15 * time.Second
	screenshotTimeout = 25 * time.Second
	quickTimeout      = 12 * time.Second
)

var errBrowserClosed = errors.New("browser session was closed")

// ---------------------------------------------------------------------------
// Per-session browser tab
// ---------------------------------------------------------------------------
// The *browserSession object is STABLE for the lifetime of an agent session —
// only its internal chromedp context is swapped out on reconnect. This keeps
// the serialization mutex tied to the sessionID so racing tool calls always
// synchronise on the same lock, and a dropped connection can be healed in place
// without callers ever seeing a different object.
type browserSession struct {
	mu      sync.Mutex // serialises EVERY operation on this tab (incl. reconnect)
	ctx     context.Context
	cancel  context.CancelFunc
	lastURL string // most recent successful navigation, replayed after a reconnect
	closed  bool
}

// sessions holds exactly one *browserSession per agent session (key: sessionID).
var sessions sync.Map

// getSession returns the stable session object for id, creating an unconnected
// placeholder if none exists. The chromedp context is established lazily under
// the session lock in ensureCtxLocked, so this never fails and never blocks.
func getSession(id string) *browserSession {
	if v, ok := sessions.Load(id); ok {
		return v.(*browserSession)
	}
	s := &browserSession{}
	actual, _ := sessions.LoadOrStore(id, s)
	return actual.(*browserSession)
}

// ensureCtxLocked guarantees a live chromedp context. Caller holds s.mu.
func (s *browserSession) ensureCtxLocked() error {
	if s.closed {
		return errBrowserClosed
	}
	if s.ctx != nil && s.ctx.Err() == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.ctx = nil

	initAllocator()
	ctx, cancel := chromedp.NewContext(allocCtx)
	// Force the browser process + first tab to exist so the context is usable.
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		return fmt.Errorf("could not launch browser (is Google Chrome installed?): %w", err)
	}
	// Enforce a fullscreen window. The --start-fullscreen launch flag covers the
	// first process, but a reconnect creates a fresh tab/window in the running
	// process where the flag no longer applies — so we set the window bounds over
	// CDP every time. Best-effort: never fail a browser op just because the window
	// couldn't be resized (e.g. headless CI, unusual window managers).
	setWindowFullscreen(ctx)
	s.ctx = ctx
	s.cancel = cancel
	return nil
}

// setWindowFullscreen puts the tab's browser window into fullscreen via CDP.
// Best-effort — errors are swallowed so an unresizable environment doesn't break
// the browser tools.
func setWindowFullscreen(ctx context.Context) {
	_ = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		winID, _, err := browser.GetWindowForTarget().Do(ctx)
		if err != nil {
			return nil
		}
		// Reset to normal first: SetWindowBounds can no-op if the window is already
		// flagged in a special state from the launch flag.
		_ = browser.SetWindowBounds(winID, &browser.Bounds{WindowState: browser.WindowStateNormal}).Do(ctx)
		return browser.SetWindowBounds(winID, &browser.Bounds{WindowState: browser.WindowStateFullscreen}).Do(ctx)
	}))
}

// reconnectLocked tears down a dead context and builds a fresh one. Caller holds s.mu.
func (s *browserSession) reconnectLocked() error {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.ctx = nil
	return s.ensureCtxLocked()
}

// runLocked executes a batch of chromedp actions with a timeout, and cancels
// promptly if the agent's context (parent) is cancelled — so a user "stop"
// interrupts an in-flight browser op instead of waiting out the timeout.
// Caller holds s.mu.
func (s *browserSession) runLocked(parent context.Context, timeout time.Duration, actions ...chromedp.Action) error {
	tctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()
	if parent != nil {
		stop := context.AfterFunc(parent, cancel)
		defer stop()
	}
	return chromedp.Run(tctx, actions...)
}

// withBrowser is the single resilient entry point for every browser tool.
//
// It serialises on the session lock, ensures a live tab, runs the built actions,
// and heals transient failures:
//   - Connection/tab-crash errors  → rebuild the tab, replay lastURL (when
//     restore=true), then retry — transparently to the agent.
//   - Element/timing errors        → one short-backoff retry (covers SPA
//     re-render races) before giving up.
//
// build() is a thunk so each retry gets a fresh action slice (output pointers
// are reset by the caller inside the thunk).
func withBrowser(ctx context.Context, sessionID string, timeout time.Duration, restore bool, build func() []chromedp.Action) error {
	s := getSession(sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureCtxLocked(); err != nil {
		return err
	}

	const maxAttempts = 4
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}

		err = s.runLocked(ctx, timeout, build()...)
		if err == nil {
			return nil
		}
		// Agent was cancelled (user stop) — surface it, don't retry.
		if ctx != nil && ctx.Err() != nil {
			return err
		}

		if isConnErr(err) {
			// The tab/browser dropped. Rebuild it and, for stateful ops, restore
			// the page we were on before retrying.
			if rerr := s.reconnectLocked(); rerr != nil {
				return err
			}
			if restore && s.lastURL != "" {
				_ = s.runLocked(ctx, navTimeout, navigateTasks(s.lastURL)...)
			}
			continue
		}

		// Non-connection failure (element not ready yet, stale node from an SPA
		// re-render, …). Retry once after a brief settle; don't spin on a
		// genuinely-absent element.
		if attempt == 0 {
			time.Sleep(350 * time.Millisecond)
			continue
		}
		break
	}
	return err
}

// isConnErr reports whether err indicates a lost CDP connection / dead tab,
// as opposed to a normal timeout or a missing element. A per-op timeout
// ("context deadline exceeded") is deliberately NOT treated as a disconnect —
// reconnecting wouldn't help a slow page and would just retry the timeout.
func isConnErr(err error) bool {
	if err == nil {
		return false
	}
	e := strings.ToLower(err.Error())
	for _, sub := range []string{
		"context canceled",
		"websocket",
		"connection refused",
		"connection reset",
		"broken pipe",
		"unexpected eof",
		"target closed",
		"not connected",
		"no such target",
		"session with given id not found",
		"page crash",
		"browser has crashed",
		"could not dial",
		"invalid context",
	} {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

// DestroyBrowserSession closes a session's tab and removes it. Called from
// browser_close and from session teardown (delete_session / sub-agent exit).
func DestroyBrowserSession(sessionID string) {
	if v, ok := sessions.LoadAndDelete(sessionID); ok {
		s := v.(*browserSession)
		s.mu.Lock()
		s.closed = true
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
		s.ctx = nil
		s.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func str(v any) string { s, _ := v.(string); return s }

// jsStr safely encodes a Go string as a JS string literal for injection.
func jsStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// normalizeURL trims input and adds a scheme when none is present, so
// "example.com" and "localhost:3000" both navigate correctly. Loopback hosts
// default to http:// (dev servers rarely serve TLS); everything else to https://.
func normalizeURL(v any) string {
	u := strings.TrimSpace(str(v))
	if u == "" {
		return ""
	}
	if strings.Contains(u, "://") || strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "data:") {
		return u
	}
	lower := strings.ToLower(u)
	for _, host := range []string{"localhost", "127.0.0.1", "0.0.0.0", "[::1]"} {
		if lower == host || strings.HasPrefix(lower, host+":") || strings.HasPrefix(lower, host+"/") {
			return "http://" + u
		}
	}
	return "https://" + u
}

// friendly rewrites opaque chromedp errors into guidance the agent can act on.
func friendly(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"):
		return "timed out — the element may not exist, is hidden, or the page is still loading. Try browser_get_inputs / browser_get_content to inspect the page, or browser_wait_for the selector first."
	case isConnErr(err):
		return "the browser connection dropped and could not be restored: " + s
	default:
		return s
	}
}

// ---------------------------------------------------------------------------
// Robust action builders
// ---------------------------------------------------------------------------

// navigateTasks navigates and waits for the DOM to be ready enough to interact.
func navigateTasks(url string) []chromedp.Action {
	return []chromedp.Action{
		chromedp.Navigate(url),
		chromedp.WaitReady("body", chromedp.ByQuery),
		waitReadyStateAction(),
	}
}

// waitReadyStateAction polls document.readyState until the document is at least
// interactive, bounded by the surrounding op timeout. Best-effort: probe errors
// don't fail the navigation (the load event already fired).
func waitReadyStateAction() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for {
			var st string
			if err := chromedp.Run(ctx, chromedp.Evaluate("document.readyState", &st)); err != nil {
				return nil
			}
			if st == "interactive" || st == "complete" {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
	})
}

// scrollIntoViewJS centers an element in the viewport before interacting.
func scrollIntoViewJS(sel string) string {
	return `(function(){var el=document.querySelector(` + jsStr(sel) + `);if(el&&el.scrollIntoView){el.scrollIntoView({block:'center',inline:'center'});}})()`
}

// clearValueJS empties an input/textarea/contenteditable using the native value
// setter so framework-controlled inputs (React/Vue) actually register the reset,
// then fires an input event so bound state updates to empty.
func clearValueJS(sel string) string {
	return `(function(){var el=document.querySelector(` + jsStr(sel) + `);if(!el)return;el.focus();try{if(el.isContentEditable){el.textContent='';}else{var proto=el instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;var d=Object.getOwnPropertyDescriptor(proto,'value');if(d&&d.set){d.set.call(el,'');}else{el.value='';}}el.dispatchEvent(new Event('input',{bubbles:true}));}catch(e){}})()`
}

// dispatchInputJS fires input+change so frameworks commit the typed value.
func dispatchInputJS(sel string) string {
	return `(function(){var el=document.querySelector(` + jsStr(sel) + `);if(!el)return;el.dispatchEvent(new Event('input',{bubbles:true}));el.dispatchEvent(new Event('change',{bubbles:true}));})()`
}

// clickTasks waits for the element to be visible, scrolls it into view, then clicks.
func clickTasks(sel string) []chromedp.Action {
	return []chromedp.Action{
		chromedp.WaitVisible(sel, chromedp.ByQuery),
		chromedp.Evaluate(scrollIntoViewJS(sel), nil),
		chromedp.Click(sel, chromedp.ByQuery),
	}
}

// typeTasks reliably enters text: wait-visible → scroll → focus → (clear via
// native setter) → real keystrokes → input/change events.
func typeTasks(sel, text string, clear bool) []chromedp.Action {
	tasks := []chromedp.Action{
		chromedp.WaitVisible(sel, chromedp.ByQuery),
		chromedp.Evaluate(scrollIntoViewJS(sel), nil),
		chromedp.Focus(sel, chromedp.ByQuery),
	}
	if clear {
		tasks = append(tasks, chromedp.Evaluate(clearValueJS(sel), nil))
	}
	tasks = append(tasks,
		chromedp.SendKeys(sel, text, chromedp.ByQuery),
		chromedp.Evaluate(dispatchInputJS(sel), nil),
	)
	return tasks
}

// ---------------------------------------------------------------------------
// Branded control banner
// ---------------------------------------------------------------------------

const tollecodeBannerScript = `
(function() {
  var id = '__tollecode_banner__';
  if (document.getElementById(id)) return;
  var bar = document.createElement('div');
  bar.id = id;
  bar.style.cssText = [
    'position:fixed','top:0','left:0','right:0','z-index:2147483647',
    'background:linear-gradient(90deg,#7C5CF5,#5B8DEF)',
    'color:#fff','font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif',
    'font-size:11px','font-weight:500','letter-spacing:0.3px',
    'padding:3px 10px','text-align:center','pointer-events:none','user-select:none',
    'box-shadow:0 1px 4px rgba(0,0,0,.25)',
  ].join(';');
  bar.textContent = 'Tollecode is controlling this browser';
  document.documentElement.insertBefore(bar, document.body || null);
})();
`

// ---------------------------------------------------------------------------
// Tool implementations (signatures unchanged — dispatched from tools_registry.go)
// ---------------------------------------------------------------------------

func toolBrowserNavigate(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	url := normalizeURL(inp["url"])
	if url == "" {
		return "Error: 'url' is required.", true
	}

	err := withBrowser(ctx, cfg.SessionID, navTimeout, false, func() []chromedp.Action {
		return navigateTasks(url)
	})
	if err != nil {
		return "Error navigating to " + url + ": " + friendly(err), true
	}

	// Remember the page so a mid-session reconnect can restore it.
	s := getSession(cfg.SessionID)
	s.mu.Lock()
	s.lastURL = url
	s.mu.Unlock()

	// Best-effort branding — never fails the navigation.
	_ = withBrowser(ctx, cfg.SessionID, quickTimeout, false, func() []chromedp.Action {
		return []chromedp.Action{chromedp.Evaluate(tollecodeBannerScript, nil)}
	})
	return "Navigated to " + url, false
}

func toolBrowserScreenshot(ctx context.Context, cfg *Config) (string, string, bool) {
	var buf []byte
	err := withBrowser(ctx, cfg.SessionID, screenshotTimeout, true, func() []chromedp.Action {
		buf = nil
		// quality=0 → PNG (the UI renders data:image/png;base64,…).
		return []chromedp.Action{chromedp.FullScreenshot(&buf, 0)}
	})
	if err != nil {
		return "Error taking browser screenshot: " + friendly(err), "", true
	}

	encoded := base64.StdEncoding.EncodeToString(buf)

	// Extract dimensions from the PNG IHDR (bytes 16–23).
	var width, height int
	if len(buf) >= 24 {
		width = int(buf[16])<<24 | int(buf[17])<<16 | int(buf[18])<<8 | int(buf[19])
		height = int(buf[20])<<24 | int(buf[21])<<16 | int(buf[22])<<8 | int(buf[23])
	}

	emitFn := cfg.EmitEvent
	if emitFn == nil {
		emitFn = cfg.EmitFn
	}
	if emitFn != nil {
		emitFn(map[string]any{
			"type":   "screen_event",
			"action": "browser_screenshot",
			"image":  encoded,
			"width":  width,
			"height": height,
		})
	}
	return fmt.Sprintf("Browser screenshot captured (%d×%d px).", width, height), encoded, false
}

func toolBrowserClick(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	selector := str(inp["selector"])
	if selector == "" {
		return "Error: 'selector' is required.", true
	}
	err := withBrowser(ctx, cfg.SessionID, interactTimeout, true, func() []chromedp.Action {
		return clickTasks(selector)
	})
	if err != nil {
		return "Error clicking " + selector + ": " + friendly(err), true
	}
	return "Clicked " + selector, false
}

func toolBrowserType(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	selector := str(inp["selector"])
	text := str(inp["text"])
	if selector == "" {
		return "Error: 'selector' is required.", true
	}
	// Default to replacing the field's contents; opt out with clear=false to append.
	clear := true
	if c, ok := inp["clear"].(bool); ok {
		clear = c
	}

	err := withBrowser(ctx, cfg.SessionID, interactTimeout, true, func() []chromedp.Action {
		return typeTasks(selector, text, clear)
	})
	if err != nil {
		return "Error typing into " + selector + ": " + friendly(err), true
	}
	verb := "Typed"
	if !clear {
		verb = "Appended"
	}
	return fmt.Sprintf("%s %q into %s", verb, text, selector), false
}

func toolBrowserKeyPress(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	key := str(inp["key"])
	if key == "" {
		return "Error: 'key' is required.", true
	}
	err := withBrowser(ctx, cfg.SessionID, quickTimeout, true, func() []chromedp.Action {
		return []chromedp.Action{chromedp.KeyEvent(key)}
	})
	if err != nil {
		return "Error pressing key " + key + ": " + friendly(err), true
	}
	return "Pressed " + key, false
}

func toolBrowserEvaluate(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	script := str(inp["script"])
	if script == "" {
		return "Error: 'script' is required.", true
	}
	var result any
	err := withBrowser(ctx, cfg.SessionID, interactTimeout, true, func() []chromedp.Action {
		result = nil
		return []chromedp.Action{chromedp.Evaluate(script, &result)}
	})
	if err != nil {
		return "Error evaluating script: " + friendly(err), true
	}
	out, _ := json.Marshal(result)
	return string(out), false
}

func toolBrowserGetContent(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	selector := "body"
	if sel := str(inp["selector"]); sel != "" {
		selector = sel
	}
	var html string
	err := withBrowser(ctx, cfg.SessionID, interactTimeout, true, func() []chromedp.Action {
		html = ""
		return []chromedp.Action{chromedp.OuterHTML(selector, &html, chromedp.ByQuery)}
	})
	if err != nil {
		return "Error getting content of " + selector + ": " + friendly(err), true
	}
	if len(html) > 12000 {
		html = html[:12000] + "\n... (truncated)"
	}
	return html, false
}

func toolBrowserWaitFor(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	selector := str(inp["selector"])
	if selector == "" {
		return "Error: 'selector' is required.", true
	}
	err := withBrowser(ctx, cfg.SessionID, interactTimeout, true, func() []chromedp.Action {
		return []chromedp.Action{chromedp.WaitVisible(selector, chromedp.ByQuery)}
	})
	if err != nil {
		return "Error waiting for " + selector + ": " + friendly(err), true
	}
	return selector + " is visible", false
}

func toolBrowserGetInputs(ctx context.Context, cfg *Config) (string, bool) {
	// Extract all interactive elements: inputs, textareas, selects, buttons.
	// Returns a JSON array with selector, type, label, placeholder, value, visible.
	script := `
(function() {
  function bestSelector(el) {
    if (el.id) return '#' + CSS.escape(el.id);
    if (el.name) return el.tagName.toLowerCase() + '[name="' + el.name + '"]';
    const path = [];
    let cur = el;
    while (cur && cur !== document.body) {
      let seg = cur.tagName.toLowerCase();
      if (cur.id) { seg = '#' + CSS.escape(cur.id); path.unshift(seg); break; }
      const idx = Array.from(cur.parentElement?.children || []).indexOf(cur) + 1;
      seg += ':nth-child(' + idx + ')';
      path.unshift(seg);
      cur = cur.parentElement;
    }
    return path.join(' > ');
  }
  function labelFor(el) {
    if (el.id) {
      const lbl = document.querySelector('label[for="' + CSS.escape(el.id) + '"]');
      if (lbl) return lbl.innerText.trim();
    }
    const parent = el.closest('label');
    if (parent) return parent.innerText.replace(el.value || '', '').trim();
    const prev = el.previousElementSibling;
    if (prev && (prev.tagName === 'LABEL' || prev.tagName === 'SPAN' || prev.tagName === 'P'))
      return prev.innerText.trim();
    return el.getAttribute('aria-label') || '';
  }
  const results = [];
  document.querySelectorAll('input:not([type="hidden"]), textarea, select, button[type="submit"], button[type="button"]').forEach(el => {
    const rect = el.getBoundingClientRect();
    if (rect.width === 0 && rect.height === 0) return; // hidden
    results.push({
      selector: bestSelector(el),
      tag: el.tagName.toLowerCase(),
      type: el.type || el.tagName.toLowerCase(),
      label: labelFor(el),
      placeholder: el.placeholder || '',
      value: el.tagName === 'SELECT' ? el.options[el.selectedIndex]?.text || '' : (el.value || ''),
      required: el.required || false,
      disabled: el.disabled || false,
    });
  });
  return JSON.stringify(results, null, 2);
})()
`
	var result string
	err := withBrowser(ctx, cfg.SessionID, interactTimeout, true, func() []chromedp.Action {
		result = ""
		return []chromedp.Action{chromedp.Evaluate(script, &result)}
	})
	if err != nil {
		return "Error extracting inputs: " + friendly(err), true
	}
	if result == "" || result == "null" {
		return "No interactive inputs found on the current page.", false
	}
	if len(result) > 8000 {
		result = result[:8000] + "\n... (truncated)"
	}
	return result, false
}

func toolBrowserClose(ctx context.Context, cfg *Config) (string, bool) {
	DestroyBrowserSession(cfg.SessionID)
	return "Browser session closed.", false
}
