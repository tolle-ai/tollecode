package ai

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/tolle-ai/tollecode/internal/config"
)

// Egress guardrail: scans outbound LLM requests for secrets/PII before they leave
// the machine. Every adapter built by buildAdapter is wrapped by scanningProvider,
// so the check is uniform and cannot be bypassed by choosing a provider.
//
// Posture is observe-first: the default mode is EgressLog, which flags what WOULD
// be redacted without altering the request, so operators can tune detectors before
// switching to EgressRedact. EgressOff disables scanning entirely.
type EgressMode string

const (
	EgressOff    EgressMode = "off"
	EgressLog    EgressMode = "log"
	EgressRedact EgressMode = "redact"
)

// egressPolicy is the active mode. Default is observe-first logging. It is applied
// from SidecarSettings via SyncEgressFromSettings (at startup and on settings
// change), and read on every Stream call so a runtime change takes effect without
// rebuilding adapters. Guarded because the settings handler writes it from one
// goroutine while in-flight Stream calls read it from others.
var (
	egressMu     sync.RWMutex
	egressPolicy = EgressLog
)

// CurrentEgressPolicy returns the active guardrail mode.
func CurrentEgressPolicy() EgressMode {
	egressMu.RLock()
	defer egressMu.RUnlock()
	return egressPolicy
}

// SetEgressPolicy sets the active guardrail mode.
func SetEgressPolicy(m EgressMode) {
	egressMu.Lock()
	egressPolicy = m
	egressMu.Unlock()
}

// SyncEgressFromSettings applies the guardrail mode from the persisted sidecar
// settings to EgressPolicy. Called at startup and whenever settings change so a
// stored or updated mode takes effect on the next request. (ai already depends on
// config, so this keeps the wiring out of the startup callers.)
//
// The TOLLECODE_EGRESS environment variable overrides the persisted setting when
// set to off/log/redact — convenient for a one-off CLI run, e.g.
// `TOLLECODE_EGRESS=off tollecode`.
func SyncEgressFromSettings() {
	mode := config.GetSidecarSettings().EffectiveEgressMode()
	if env := strings.ToLower(strings.TrimSpace(os.Getenv("TOLLECODE_EGRESS"))); env != "" {
		switch env {
		case "off", "log", "redact":
			mode = env
		}
	}
	switch mode {
	case "off":
		SetEgressPolicy(EgressOff)
	case "redact":
		SetEgressPolicy(EgressRedact)
	default:
		SetEgressPolicy(EgressLog)
	}
}

// EgressSink receives the aggregated findings for one outbound request. The
// default logs a masked summary to stderr; the app may override it to route
// findings to the UI, the audit log, or a metrics counter.
var EgressSink = func(findings []EgressFinding, model string) {
	parts := make([]string, len(findings))
	for i, f := range findings {
		parts[i] = fmt.Sprintf("%s×%d", f.Type, f.Count)
	}
	fmt.Fprintf(os.Stderr, "[egress-guardrail] outbound request to model %q contains potential secrets/PII: %s (mode=%s)\n",
		model, strings.Join(parts, ", "), CurrentEgressPolicy())
}

// EgressFinding aggregates one detector's hits in a request. Sample is masked and
// never contains the raw secret.
type EgressFinding struct {
	Type   string `json:"type"`
	Count  int    `json:"count"`
	Sample string `json:"sample"`
}

var egressDetectors = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private_key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`)},
	{"aws_access_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"gcp_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"openai_key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)},
	{"github_token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{"stripe_key", regexp.MustCompile(`\b(?:sk|rk)_live_[A-Za-z0-9]{16,}\b`)},
	{"gitlab_token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}\b`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`)},
	{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
}

// ScanText returns the detector hits in a single string, keyed by detector.
func scanInto(text string, counts map[string]int, samples map[string]string) {
	if text == "" {
		return
	}
	for _, d := range egressDetectors {
		matches := d.re.FindAllString(text, -1)
		if len(matches) == 0 {
			continue
		}
		counts[d.name] += len(matches)
		if _, ok := samples[d.name]; !ok {
			samples[d.name] = maskSecret(matches[0])
		}
	}
}

// scanAny walks an arbitrary decoded-JSON value (tool-call input) and scans every
// string it contains, at any depth.
func scanAny(val any, counts map[string]int, samples map[string]string) {
	switch t := val.(type) {
	case string:
		scanInto(t, counts, samples)
	case []any:
		for _, e := range t {
			scanAny(e, counts, samples)
		}
	case map[string]any:
		for _, e := range t {
			scanAny(e, counts, samples)
		}
	}
}

// scanRequest walks everything that would leave the machine — the system prompt,
// every message body, every tool-result body, and every tool-call argument — and
// aggregates findings. Tool-call arguments matter as much as results: an agent
// that read a secret before the guardrail was armed will pass it straight back as
// an argument, and that turn is replayed on every subsequent request.
func scanRequest(req StreamRequest) []EgressFinding {
	counts := map[string]int{}
	samples := map[string]string{}
	scanInto(req.System, counts, samples)
	for _, m := range req.Messages {
		scanInto(m.Content, counts, samples)
		for _, tr := range m.ToolResults {
			scanInto(tr.Content, counts, samples)
		}
		for _, tc := range m.ToolCalls {
			scanAny(tc.Input, counts, samples)
		}
	}
	if len(counts) == 0 {
		return nil
	}
	findings := make([]EgressFinding, 0, len(counts))
	for name, n := range counts {
		findings = append(findings, EgressFinding{Type: name, Count: n, Sample: samples[name]})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Type < findings[j].Type })
	return findings
}

// maskSecret renders a non-reversible preview: a short prefix plus the length,
// so logs are useful for identification without themselves leaking the value.
func maskSecret(s string) string {
	prefix := s
	if len(prefix) > 4 {
		prefix = prefix[:4]
	}
	return fmt.Sprintf("%s…(%d)", prefix, len(s))
}

// secretHandleBriefing is appended to the system prompt whenever redaction is
// active with a vault. Without it the model treats a handle as corrupt data —
// it apologises, refuses, or invents a plausible-looking key. With it, the model
// knows to pass the handle through untouched and that doing so is sufficient.
const secretHandleBriefing = "\n\n## Secret handles\n" +
	"Secrets in this conversation are replaced with handles of the form " +
	"${TOLLE_SECRET_<type>_<id>}. The real value is held on the local machine and " +
	"is substituted back in automatically, immediately before any tool runs.\n" +
	"- Use a handle exactly where the real secret would go, copied verbatim.\n" +
	"- Do NOT ask the user for the real value, guess it, or invent a replacement — " +
	"the handle is all you need and the call will succeed.\n" +
	"- Do NOT rewrite a handle into a file expecting the literal text to persist; " +
	"write it only where the real secret is meant to end up.\n" +
	"- Two different handles are two different secrets; identical handles are the same secret."

// redactText replaces every detector match with a reversible vault alias, so the
// model can refer to a secret it never sees. With no vault the alias cannot be
// resolved later, so we fall back to a non-reversible type label.
func redactText(text string, v *SecretVault) string {
	if text == "" {
		return text
	}
	for _, d := range egressDetectors {
		name := d.name
		text = d.re.ReplaceAllStringFunc(text, func(match string) string {
			if v == nil {
				return "[REDACTED:" + name + "]"
			}
			return v.alias(name, match)
		})
	}
	return text
}

func redactAny(val any, v *SecretVault) any {
	switch t := val.(type) {
	case string:
		return redactText(t, v)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactAny(e, v)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = redactAny(e, v)
		}
		return out
	default:
		return val
	}
}

// redactRequest returns a copy of req with secrets/PII swapped for vault aliases.
// It copies the message, tool-result, and tool-call slices so the caller's
// in-memory history (which is persisted, and which local tool execution reads)
// is never mutated — only the bytes on the wire change.
func redactRequest(req StreamRequest, v *SecretVault) StreamRequest {
	out := req
	out.System = redactText(req.System, v)
	if v != nil {
		out.System += secretHandleBriefing
	}
	out.Messages = make([]ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		m.Content = redactText(m.Content, v)
		if len(m.ToolResults) > 0 {
			trs := make([]ToolResult, len(m.ToolResults))
			for j, tr := range m.ToolResults {
				tr.Content = redactText(tr.Content, v)
				trs[j] = tr
			}
			m.ToolResults = trs
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				if red, ok := redactAny(tc.Input, v).(map[string]any); ok {
					tc.Input = red
				}
				tcs[j] = tc
			}
			m.ToolCalls = tcs
		}
		out.Messages[i] = m
	}
	return out
}

// scanningProvider decorates a Provider with the egress guardrail.
type scanningProvider struct {
	inner Provider
}

func (p *scanningProvider) Stream(ctx context.Context, req StreamRequest) (<-chan StreamEvent, error) {
	policy := CurrentEgressPolicy()
	if policy == EgressOff {
		return p.inner.Stream(ctx, req)
	}
	if findings := scanRequest(req); len(findings) > 0 {
		if EgressSink != nil {
			EgressSink(findings, req.Model)
		}
		if policy == EgressRedact {
			req = redactRequest(req, SecretVaultFrom(ctx))
		}
	}
	return p.inner.Stream(ctx, req)
}

func (p *scanningProvider) DiscoverModels(ctx context.Context) ([]ModelInfo, error) {
	return p.inner.DiscoverModels(ctx)
}
