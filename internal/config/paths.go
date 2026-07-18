package config

import (
	"os"
	"path/filepath"
)

var devMode bool

// SetDevMode switches the data directory to ~/.tollecode-dev.
func SetDevMode() {
	devMode = true
	os.Setenv("TOLLECODE_ENV", "development")
	os.Setenv("TOLLECODE_HOME", filepath.Join(homeDir(), ".tollecode-dev"))
}

// Home returns the Tollecode data directory (~/.tollecode or ~/.tollecode-dev).
func Home() string {
	if v := os.Getenv("TOLLECODE_HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".tollecode")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	// HOME env var is often set in containers even when getpwuid fails
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	// Last resort: use /tmp/tollecode as a writable data directory.
	// This ensures persistence still works in containers without /etc/passwd.
	return "/tmp/tollecode"
}
