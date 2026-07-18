package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)


// toolScreenshot captures the current screen.
// When cfg.TakeScreenshot is set (Tauri/desktop-app mode) it delegates to that
// callback. In CLI mode it falls back to NativeCaptureScreen.
// Also populates cfg.lastScreen* fields used for HiDPI coordinate scaling.
func toolScreenshot(ctx context.Context, cfg *Config) (string, string, bool) {
	var imageData string
	var width, height int

	if cfg.TakeScreenshot != nil {
		payload, err := cfg.TakeScreenshot(ctx)
		if err != nil {
			return "Error taking screenshot: " + err.Error(), "", true
		}
		imageData, _ = payload["image"].(string)
		if w, ok := payload["width"].(int); ok {
			width = w
		}
		if h, ok := payload["height"].(int); ok {
			height = h
		}
		// Tauri also returns realWidth/realHeight (physical pixels before resize).
		// Use them together with logical screen dims to compute coordinate scale.
		if rw, ok := payload["realWidth"].(int); ok && rw > 0 && width > 0 {
			logW, logH := getLogicalScreenSize()
			if logW > 0 {
				cfg.lastScreenImgW = width
				cfg.lastScreenImgH = height
				cfg.lastScreenLogicalW = logW
				cfg.lastScreenLogicalH = logH
			}
		}
	} else {
		// CLI mode: use OS-native capture of the primary display only.
		var err error
		imageData, width, height, err = NativeCaptureScreen()
		if err != nil {
			return "Error taking screenshot: " + err.Error(), "", true
		}
		// In CLI mode the captured image is physical-pixel resolution;
		// get logical dims so mouse clicks scale correctly.
		if width > 0 {
			logW, logH := getLogicalScreenSize()
			if logW > 0 {
				cfg.lastScreenImgW = width
				cfg.lastScreenImgH = height
				cfg.lastScreenLogicalW = logW
				cfg.lastScreenLogicalH = logH
			}
		}
	}

	emitFn := cfg.EmitEvent
	if emitFn == nil {
		emitFn = cfg.EmitFn
	}
	if emitFn != nil {
		emitFn(map[string]any{
			"type":   "screen_event",
			"action": "screenshot",
			"image":  imageData,
			"width":  width,
			"height": height,
		})
	}

	return fmt.Sprintf("Screenshot captured (%d×%d px). Use these image dimensions for all x/y coordinates.", width, height), imageData, false
}

// NativeCaptureScreen takes a screenshot of the PRIMARY display only using
// built-in OS tools (no external dependencies).
// Returns base64-encoded PNG, width, height.
// Exported so the CLI can use it as a TakeScreenshot callback for /screen.
func NativeCaptureScreen() (imageData string, width, height int, err error) {
	tmpFile := fmt.Sprintf("/tmp/toll_screen_%d.png", time.Now().UnixNano())
	defer os.Remove(tmpFile)

	switch runtime.GOOS {
	case "darwin":
		// -x = silent (no shutter sound), -D 1 = primary display only.
		if err = exec.Command("screencapture", "-x", "-D", "1", tmpFile).Run(); err != nil {
			return "", 0, 0, fmt.Errorf("screencapture failed: %w", err)
		}
	case "linux":
		if _, lookErr := exec.LookPath("scrot"); lookErr == nil {
			if err = exec.Command("scrot", "-z", tmpFile).Run(); err != nil {
				return "", 0, 0, fmt.Errorf("scrot failed: %w", err)
			}
		} else if _, lookErr = exec.LookPath("import"); lookErr == nil {
			if err = exec.Command("import", "-window", "root", tmpFile).Run(); err != nil {
				return "", 0, 0, fmt.Errorf("imagemagick import failed: %w", err)
			}
		} else {
			return "", 0, 0, fmt.Errorf("no screenshot tool found; install scrot: sudo apt install scrot")
		}
	default:
		return "", 0, 0, fmt.Errorf("native screenshot not supported on %s", runtime.GOOS)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return "", 0, 0, fmt.Errorf("reading screenshot: %w", err)
	}

	imageData = base64.StdEncoding.EncodeToString(data)

	// Extract dimensions from PNG IHDR chunk (bytes 16-24).
	if len(data) >= 24 {
		width = int(data[16])<<24 | int(data[17])<<16 | int(data[18])<<8 | int(data[19])
		height = int(data[20])<<24 | int(data[21])<<16 | int(data[22])<<8 | int(data[23])
	}

	return imageData, width, height, nil
}
