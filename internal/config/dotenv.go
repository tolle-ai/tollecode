package config

import (
	"os"
	"strings"
)

// LoadDotEnv reads $HOME/.tollecode/.env and sets any variables not already
// present in the process environment. Silently no-ops if the file is absent.
//
// This lives in config rather than selfhost because loading the user's .env is
// useful in every build — provider API keys are the common case — while selfhost
// is compiled in only under the `selfhost` build tag.
func LoadDotEnv() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(home + "/.tollecode/.env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}
