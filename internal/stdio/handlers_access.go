package stdio

import (
	"github.com/tolle-ai/tollecode/internal/liteaccess"
)

// Server access key ("the door") management over the command bus. Unlike the
// pre-door access_key_status probe — which the web bridge answers connection-
// locally before a socket is authenticated — these commands administer the key
// and so run through the normal dispatch, which requires an authenticated
// (unlocked) connection. That's deliberate: only someone already through the
// door and past the TOTP lock may read, rotate, or disable it.
//
// The desktop stdio shell reaches these too (its transport is trusted local
// IPC), which is how a user enables/rotates the key from Settings on the machine
// itself before handing it to other browsers.

// handleAccessKeyGet returns the current key + enabled flag for display in
// Settings on the machine that owns it. The caller is already authenticated.
func handleAccessKeyGet(state *ServerState, cmd map[string]any) {
	Emit(map[string]any{
		"type":    "access_key_get",
		"enabled": liteaccess.Required(),
		"key":     liteaccess.Key(),
	})
}

// handleAccessKeyGenerate enables the door with a fresh key (or rotates it),
// returning the plaintext so it can be shown and copied. Rotating locks out any
// browser still holding the previous key on its next reconnect.
func handleAccessKeyGenerate(state *ServerState, cmd map[string]any) {
	key, err := liteaccess.Generate()
	if err != nil {
		Emit(map[string]any{"type": "access_key_generate", "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "access_key_generate", "enabled": true, "key": key})
}

// handleAccessKeyDisable turns the door off; web mode reverts to TOTP-only.
func handleAccessKeyDisable(state *ServerState, cmd map[string]any) {
	if err := liteaccess.Disable(); err != nil {
		Emit(map[string]any{"type": "access_key_disable", "error": err.Error()})
		return
	}
	Emit(map[string]any{"type": "access_key_disable", "enabled": false})
}
