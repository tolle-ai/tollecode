package agent

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CheckAccessibilityPermission returns true when the running process has macOS
// Accessibility permission (kTCCServiceAccessibility). Always returns true on
// non-macOS platforms — no gating is needed there.
func CheckAccessibilityPermission() bool {
	if runtime.GOOS != "darwin" {
		return true
	}
	script := `import ctypes; lib = ctypes.cdll.LoadLibrary('/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices'); print(lib.AXIsProcessTrusted())`
	for _, py := range []string{"python3", "/usr/bin/python3", "/opt/homebrew/bin/python3"} {
		out, err := exec.Command(py, "-c", script).Output()
		if err == nil {
			return strings.TrimSpace(string(out)) == "1"
		}
	}
	return false
}

// ensureAccessibility checks for macOS accessibility permission and, if absent,
// calls cfg.RequestSystemPermission to guide the user.
func ensureAccessibility(ctx context.Context, cfg *Config) (errMsg string, isErr bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	if CheckAccessibilityPermission() {
		return "", false
	}
	if cfg.RequestSystemPermission == nil {
		return "Accessibility permission is required for desktop control. " +
			"Go to System Settings › Privacy & Security › Accessibility and enable Tollecode.", true
	}
	granted := cfg.RequestSystemPermission(ctx, "accessibility")
	if !granted {
		return "Accessibility permission was denied or timed out. " +
			"Go to System Settings › Privacy & Security › Accessibility and enable Tollecode, then try again.", true
	}
	if !CheckAccessibilityPermission() {
		return "Accessibility permission was not detected after granting. " +
			"Please restart Tollecode and try again.", true
	}
	return "", false
}

// emitDesktopAction broadcasts a screen_event so channel/UI views can show the action log.
func emitDesktopAction(cfg *Config, fields map[string]any) {
	fields["type"] = "screen_event"
	emitFn := cfg.EmitEvent
	if emitFn == nil {
		emitFn = cfg.EmitFn
	}
	if emitFn != nil {
		emitFn(fields)
	}
}

// ── Mouse ─────────────────────────────────────────────────────────────────────

func toolMouseMove(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	if msg, isErr := ensureAccessibility(ctx, cfg); isErr {
		return msg, true
	}
	imgX := int(toFloat(inp["x"]))
	imgY := int(toFloat(inp["y"]))
	x, y := scaleToLogical(cfg, imgX, imgY)
	if err := desktopMouseMove(x, y); err != nil {
		return "mouse_move failed: " + err.Error(), true
	}
	emitDesktopAction(cfg, map[string]any{"action": "mouse_move", "x": x, "y": y})
	return fmt.Sprintf("Mouse moved to (%d, %d).", x, y), false
}

func toolMouseClick(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	if msg, isErr := ensureAccessibility(ctx, cfg); isErr {
		return msg, true
	}
	button, _ := inp["button"].(string)
	if button == "" {
		button = "left"
	}
	double, _ := inp["double"].(bool)

	xi, hasX := inp["x"]
	yi, hasY := inp["y"]
	var logicalX, logicalY int
	if hasX && hasY {
		logicalX, logicalY = scaleToLogical(cfg, int(toFloat(xi)), int(toFloat(yi)))
		if err := desktopMouseMove(logicalX, logicalY); err != nil {
			return "mouse_move failed: " + err.Error(), true
		}
		// Give the OS time to focus the element under the cursor.
		time.Sleep(120 * time.Millisecond)
	}

	if err := desktopMouseClick(button, double); err != nil {
		return "mouse_click failed: " + err.Error(), true
	}

	// Brief settle so focus/selection state is stable before the next action.
	time.Sleep(80 * time.Millisecond)

	label := button + " click"
	if double {
		label = "double " + label
	}
	action := map[string]any{"action": "mouse_click", "button": button, "double": double}
	if hasX && hasY {
		action["x"] = logicalX
		action["y"] = logicalY
		label += fmt.Sprintf(" at (%d, %d)", logicalX, logicalY)
	}
	emitDesktopAction(cfg, action)
	return label + ".", false
}

// ── Keyboard ──────────────────────────────────────────────────────────────────

func toolKeyboardType(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	if msg, isErr := ensureAccessibility(ctx, cfg); isErr {
		return msg, true
	}
	text, _ := inp["text"].(string)
	if text == "" {
		return "text is required", true
	}
	if err := desktopTypeText(text); err != nil {
		return "keyboard_type failed: " + err.Error(), true
	}
	emitDesktopAction(cfg, map[string]any{"action": "keyboard_type", "text": text})
	preview := text
	if len(preview) > 40 {
		preview = preview[:40] + "…"
	}
	return fmt.Sprintf("Typed: %q", preview), false
}

func toolKeyPress(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	if msg, isErr := ensureAccessibility(ctx, cfg); isErr {
		return msg, true
	}
	key, _ := inp["key"].(string)
	if key == "" {
		return "key is required", true
	}
	if err := desktopKeyPress(key); err != nil {
		return "key_press failed: " + err.Error(), true
	}
	emitDesktopAction(cfg, map[string]any{"action": "key_press", "key": key})
	return fmt.Sprintf("Pressed key: %s", key), false
}

func toolScrollMouse(ctx context.Context, cfg *Config, inp map[string]any) (string, bool) {
	if msg, isErr := ensureAccessibility(ctx, cfg); isErr {
		return msg, true
	}
	direction, _ := inp["direction"].(string)
	if direction == "" {
		direction = "down"
	}
	amount := int(toFloat(inp["amount"]))
	if amount <= 0 {
		amount = 3
	}
	if err := desktopScroll(direction, amount); err != nil {
		return "scroll failed: " + err.Error(), true
	}
	emitDesktopAction(cfg, map[string]any{"action": "scroll", "direction": direction, "amount": amount})
	return fmt.Sprintf("Scrolled %s %d units.", direction, amount), false
}

// ── OS-level implementations ──────────────────────────────────────────────────

func desktopMouseMove(x, y int) error {
	switch runtime.GOOS {
	case "darwin":
		return runPython3(darwinMouseScript("move", x, y, "left", false))
	case "linux":
		return runCmd("xdotool", "mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y))
	default:
		return fmt.Errorf("mouse control not supported on %s", runtime.GOOS)
	}
}

func desktopMouseClick(button string, double bool) error {
	switch runtime.GOOS {
	case "darwin":
		return runPython3(darwinMouseScript("click", -1, -1, button, double))
	case "linux":
		btn := linuxButton(button)
		if double {
			_ = runCmd("xdotool", "click", "--repeat", "2", btn)
			return nil
		}
		return runCmd("xdotool", "click", btn)
	default:
		return fmt.Errorf("mouse control not supported on %s", runtime.GOOS)
	}
}

func desktopTypeText(text string) error {
	switch runtime.GOOS {
	case "darwin":
		// osascript keystroke handles most printable ASCII; for multi-line use
		// a Python CoreGraphics approach.
		escaped := strings.ReplaceAll(text, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		script := fmt.Sprintf(`tell application "System Events" to keystroke "%s"`, escaped)
		return runOsascript(script)
	case "linux":
		return runCmd("xdotool", "type", "--clearmodifiers", "--", text)
	default:
		return fmt.Errorf("keyboard type not supported on %s", runtime.GOOS)
	}
}

func desktopKeyPress(key string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`tell application "System Events" to %s`, darwinKeystroke(key))
		return runOsascript(script)
	case "linux":
		xKey := linuxKey(key)
		return runCmd("xdotool", "key", "--clearmodifiers", xKey)
	default:
		return fmt.Errorf("key press not supported on %s", runtime.GOOS)
	}
}

func desktopScroll(direction string, amount int) error {
	switch runtime.GOOS {
	case "darwin":
		sign := amount
		if direction == "down" {
			sign = -amount
		}
		return runPython3(darwinScrollScript(sign))
	case "linux":
		btn := "5" // scroll down
		if direction == "up" {
			btn = "4"
		}
		for i := 0; i < amount; i++ {
			if err := runCmd("xdotool", "click", btn); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("scroll not supported on %s", runtime.GOOS)
	}
}

// ── macOS helpers ─────────────────────────────────────────────────────────────

// darwinMouseScript returns a Python3+CoreGraphics script to move/click the mouse.
// If x < 0, uses the current cursor position (click-at-current).
func darwinMouseScript(action string, x, y int, button string, double bool) string {
	btnConst := "kCGMouseButtonLeft"
	downEvt := "kCGEventLeftMouseDown"
	upEvt := "kCGEventLeftMouseUp"
	if button == "right" {
		btnConst = "kCGMouseButtonRight"
		downEvt = "kCGEventRightMouseDown"
		upEvt = "kCGEventRightMouseUp"
	}

	getPos := ""
	if x < 0 {
		// Use current mouse position
		getPos = `
cur = cg.CGEventCreate(None)
pos = cg.CGEventGetLocation(cur)
x = pos.x; y = pos.y
cg.CFRelease(cur)
`
	} else {
		getPos = fmt.Sprintf("x = %d; y = %d", x, y)
	}

	click := ""
	if action == "move" || action == "click" {
		click = fmt.Sprintf(`
pt = CGPoint(x, y)
if action in ('move','click'):
    ev = cg.CGEventCreateMouseEvent(None, kCGEventMouseMoved, pt, 0)
    cg.CGEventPost(kCGHIDEventTap, ev); cg.CFRelease(ev)
`, )
	}
	if action == "click" {
		times := 1
		if double {
			times = 2
		}
		click += fmt.Sprintf(`
import time
for _ in range(%d):
    ev = cg.CGEventCreateMouseEvent(None, %s, pt, %s)
    cg.CGEventPost(kCGHIDEventTap, ev); cg.CFRelease(ev)
    time.sleep(0.05)
    ev = cg.CGEventCreateMouseEvent(None, %s, pt, %s)
    cg.CGEventPost(kCGHIDEventTap, ev); cg.CFRelease(ev)
    time.sleep(0.05)
`, times, downEvt, btnConst, upEvt, btnConst)
	}

	return fmt.Sprintf(`
import ctypes
cg = ctypes.CDLL('/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics')
class CGPoint(ctypes.Structure):
    _fields_ = [('x', ctypes.c_double), ('y', ctypes.c_double)]
kCGEventMouseMoved = 5
kCGEventLeftMouseDown = 1; kCGEventLeftMouseUp = 2
kCGEventRightMouseDown = 3; kCGEventRightMouseUp = 4
kCGMouseButtonLeft = 0; kCGMouseButtonRight = 1
kCGHIDEventTap = 0
cg.CGEventCreate.restype = ctypes.c_void_p
cg.CGEventGetLocation.restype = CGPoint
cg.CGEventGetLocation.argtypes = [ctypes.c_void_p]
cg.CGEventCreateMouseEvent.restype = ctypes.c_void_p
cg.CGEventCreateMouseEvent.argtypes = [ctypes.c_void_p, ctypes.c_uint32, CGPoint, ctypes.c_uint32]
cg.CGEventPost.argtypes = [ctypes.c_uint32, ctypes.c_void_p]
cg.CFRelease.argtypes = [ctypes.c_void_p]
action = %q
%s
%s
`, action, getPos, click)
}

func darwinScrollScript(lines int) string {
	return fmt.Sprintf(`
import ctypes
cg = ctypes.CDLL('/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics')
kCGScrollEventUnitLine = 1
kCGEventScrollWheel = 22
kCGHIDEventTap = 0
cg.CGEventCreateScrollWheelEvent.restype = ctypes.c_void_p
cg.CGEventCreateScrollWheelEvent.argtypes = [ctypes.c_void_p, ctypes.c_uint32, ctypes.c_uint32, ctypes.c_int32]
cg.CGEventPost.argtypes = [ctypes.c_uint32, ctypes.c_void_p]
cg.CFRelease.argtypes = [ctypes.c_void_p]
ev = cg.CGEventCreateScrollWheelEvent(None, kCGScrollEventUnitLine, 1, %d)
cg.CGEventPost(kCGHIDEventTap, ev)
cg.CFRelease(ev)
`, lines)
}

// darwinKeyMapping maps common key names to AppleScript key codes.
var darwinKeyMapping = map[string]int{
	"return": 36, "enter": 36, "tab": 48, "space": 49, "delete": 51, "backspace": 51,
	"escape": 53, "esc": 53, "left": 123, "right": 124, "down": 125, "up": 126,
	"home": 115, "end": 119, "pageup": 116, "pagedown": 121, "f1": 122, "f2": 120,
	"f3": 99, "f4": 118, "f5": 96, "f6": 97, "f7": 98, "f8": 100,
	"f9": 101, "f10": 109, "f11": 103, "f12": 111,
}

// darwinKeyModifiers maps modifier names to AppleScript names.
var darwinKeyModifiers = map[string]string{
	"cmd": "command down", "command": "command down",
	"ctrl": "control down", "control": "control down",
	"alt": "option down", "option": "option down",
	"shift": "shift down",
}

// darwinKeystroke converts a key string (e.g. "enter", "ctrl+c", "cmd+v") to
// an AppleScript keystroke/key code expression.
func darwinKeystroke(key string) string {
	parts := strings.Split(strings.ToLower(key), "+")
	mainKey := parts[len(parts)-1]
	mods := parts[:len(parts)-1]

	modList := []string{}
	for _, m := range mods {
		if as, ok := darwinKeyModifiers[m]; ok {
			modList = append(modList, as)
		}
	}
	modSuffix := ""
	if len(modList) > 0 {
		modSuffix = " using {" + strings.Join(modList, ", ") + "}"
	}

	if code, ok := darwinKeyMapping[mainKey]; ok {
		return fmt.Sprintf("key code %d%s", code, modSuffix)
	}
	// Single printable character
	if len(mainKey) == 1 {
		escaped := strings.ReplaceAll(mainKey, `"`, `\"`)
		return fmt.Sprintf(`keystroke "%s"%s`, escaped, modSuffix)
	}
	// Unknown — try as keystroke anyway
	return fmt.Sprintf(`keystroke "%s"%s`, mainKey, modSuffix)
}

// ── Linux helpers ─────────────────────────────────────────────────────────────

func linuxButton(button string) string {
	switch strings.ToLower(button) {
	case "right":
		return "3"
	case "middle":
		return "2"
	default:
		return "1"
	}
}

// linuxKey maps common key names to xdotool key names.
func linuxKey(key string) string {
	parts := strings.Split(strings.ToLower(key), "+")
	mapped := make([]string, len(parts))
	remap := map[string]string{
		"enter": "Return", "return": "Return", "esc": "Escape", "escape": "Escape",
		"backspace": "BackSpace", "delete": "Delete", "tab": "Tab", "space": "space",
		"up": "Up", "down": "Down", "left": "Left", "right": "Right",
		"pageup": "Prior", "pagedown": "Next", "home": "Home", "end": "End",
		"cmd": "super", "command": "super",
		"ctrl": "ctrl", "control": "ctrl",
		"alt": "alt", "option": "alt",
		"shift": "shift",
	}
	for i, p := range parts {
		if v, ok := remap[p]; ok {
			mapped[i] = v
		} else {
			mapped[i] = p
		}
	}
	return strings.Join(mapped, "+")
}

// ── Command runners ───────────────────────────────────────────────────────────

func runOsascript(script string) error {
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runPython3(script string) error {
	pythons := []string{"python3", "/usr/bin/python3", "/opt/homebrew/bin/python3"}
	for _, py := range pythons {
		if _, err := exec.LookPath(filepath.Base(py)); err == nil || filepath.IsAbs(py) {
			out, err := exec.Command(py, "-c", script).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	return fmt.Errorf("python3 not found")
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ── Logical screen size cache ─────────────────────────────────────────────────

var logicalSizeOnce sync.Once
var logicalSizeCacheW, logicalSizeCacheH int

// getLogicalScreenSize returns the primary display's logical pixel dimensions.
// On macOS Retina, physical pixels = logical × scale; CGEventPost uses logical.
// Result is cached for the process lifetime (screen geometry doesn't change).
func getLogicalScreenSize() (int, int) {
	if runtime.GOOS != "darwin" {
		return 0, 0
	}
	logicalSizeOnce.Do(func() {
		script := `
import ctypes
cg = ctypes.CDLL('/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics')
cg.CGMainDisplayID.restype = ctypes.c_uint32
cg.CGDisplayPixelsWide.restype = ctypes.c_size_t
cg.CGDisplayPixelsWide.argtypes = [ctypes.c_uint32]
cg.CGDisplayPixelsHigh.restype = ctypes.c_size_t
cg.CGDisplayPixelsHigh.argtypes = [ctypes.c_uint32]
d = cg.CGMainDisplayID()
print(cg.CGDisplayPixelsWide(d), cg.CGDisplayPixelsHigh(d))
`
		out, err := exec.Command("python3", "-c", script).Output()
		if err == nil {
			fmt.Sscan(strings.TrimSpace(string(out)), &logicalSizeCacheW, &logicalSizeCacheH)
		}
	})
	return logicalSizeCacheW, logicalSizeCacheH
}

// scaleToLogical converts image-space coordinates (as seen by the LLM) to
// logical screen coordinates (as expected by CoreGraphics mouse events).
// If no screen mapping is stored in cfg the coordinates are returned unchanged.
func scaleToLogical(cfg *Config, imgX, imgY int) (int, int) {
	if cfg.lastScreenImgW <= 0 || cfg.lastScreenLogicalW <= 0 {
		return imgX, imgY
	}
	x := int(float64(imgX) * float64(cfg.lastScreenLogicalW) / float64(cfg.lastScreenImgW))
	y := int(float64(imgY) * float64(cfg.lastScreenLogicalH) / float64(cfg.lastScreenImgH))
	return x, y
}

// toFloat safely converts any numeric JSON value to float64.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
