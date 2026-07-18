package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
)

// The vault is the half of redact mode that makes the agent still work.
//
// Redaction used to replace a secret with a type label ("[REDACTED:openai_key]"),
// which is lossy and one-way: two different keys of the same type collapsed to the
// same string, and there was no way back. The model would then echo that literal
// label into a tool call — `curl -H "Authorization: Bearer [REDACTED:openai_key]"` —
// and the command ran verbatim against the shell, so every credentialed call failed
// (and a "helpful" rewrite of a .env file destroyed the real value).
//
// Instead we swap each distinct secret for a stable, reversible handle:
//
//	API_KEY=${TOLLE_SECRET_openai_key_a3f19c2b}
//
// The model never sees the plaintext, but it *can* refer to the value, and the
// executor substitutes the real secret back into tool input immediately before
// dispatch. Secrets stay on the machine; the agent keeps working.
//
// The alias is derived from the plaintext, so the same secret always yields the
// same handle — across turns, across messages, and even if a vault is rebuilt.

// SecretVault maps aliases to the plaintext they stand for, for the lifetime of
// one agent run. It is never persisted; conversation history on disk keeps the
// raw values and is re-redacted on each outbound request.
type SecretVault struct {
	mu      sync.RWMutex
	secrets map[string]string // alias -> plaintext
}

func NewSecretVault() *SecretVault {
	return &SecretVault{secrets: map[string]string{}}
}

// alias returns the stable handle for a plaintext secret, registering it on first
// sight. Derivation is a truncated SHA-256 of the plaintext: deterministic, and
// short enough that the model reliably reproduces it verbatim.
func (v *SecretVault) alias(kind, plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	a := "${TOLLE_SECRET_" + kind + "_" + hex.EncodeToString(sum[:4]) + "}"
	v.mu.Lock()
	v.secrets[a] = plaintext
	v.mu.Unlock()
	return a
}

// aliasPattern matches handles this package minted. Kept deliberately tight so
// ordinary shell `${VAR}` expansion in agent output is never touched.
var aliasPattern = regexp.MustCompile(`\$\{TOLLE_SECRET_[A-Za-z0-9_]+_[0-9a-f]{8}\}`)

// Reveal substitutes real secrets back into s for every alias the vault knows.
// Unknown aliases are left intact: a handle from an earlier session has no
// plaintext here, and failing visibly beats silently sending the literal.
func (v *SecretVault) Reveal(s string) string {
	if v == nil || s == "" || !strings.Contains(s, "${TOLLE_SECRET_") {
		return s
	}
	return aliasPattern.ReplaceAllStringFunc(s, func(a string) string {
		v.mu.RLock()
		plain, ok := v.secrets[a]
		v.mu.RUnlock()
		if !ok {
			return a
		}
		return plain
	})
}

// RevealInput walks tool-call input and reveals aliases in every string it
// contains, at any depth — a secret may arrive as a bare argument, an element of
// an args array, or a nested header map.
func (v *SecretVault) RevealInput(input map[string]any) map[string]any {
	if v == nil || len(input) == 0 {
		return input
	}
	out := make(map[string]any, len(input))
	for k, val := range input {
		out[k] = v.revealAny(val)
	}
	return out
}

func (v *SecretVault) revealAny(val any) any {
	switch t := val.(type) {
	case string:
		return v.Reveal(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = v.revealAny(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = v.revealAny(e)
		}
		return out
	default:
		return val
	}
}

type vaultCtxKey struct{}

// WithSecretVault attaches a vault to ctx. The agent executor does this once per
// run; the egress guardrail populates it as it redacts outbound requests, and the
// executor drains it when rehydrating tool input.
func WithSecretVault(ctx context.Context, v *SecretVault) context.Context {
	return context.WithValue(ctx, vaultCtxKey{}, v)
}

// SecretVaultFrom returns the vault on ctx, or nil when there is none. A nil
// vault is safe for every method here, so callers outside the agent loop (model
// discovery, one-off completions) need no special casing — they simply get
// non-reversible redaction, which is the correct behaviour when nothing on the
// far side will ever execute a tool call.
func SecretVaultFrom(ctx context.Context) *SecretVault {
	v, _ := ctx.Value(vaultCtxKey{}).(*SecretVault)
	return v
}
