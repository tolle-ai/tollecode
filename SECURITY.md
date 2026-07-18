# Security Policy

## Reporting a vulnerability

Please report security issues through GitHub's private vulnerability reporting:
**[Security → Report a vulnerability](https://github.com/tolle-ai/tollecode/security/advisories/new)**.

Do not open a public issue for a security problem. Private reporting lets us ship a fix
before the details are public.

We aim to acknowledge reports within 3 business days. If you'd like credit in the
advisory, say so in your report.

## Scope

TolleCode runs an AI agent with real access to your machine — it reads and writes files,
executes shell commands, and can drive a browser. The areas where a bug turns into a
security problem:

- **`internal/liteaccess`** — the web-mode access key. Bypassing the key, forging or
  extending a grant, or timing attacks on key comparison.
- **`internal/liteauth`** — local TOTP accounts, backup codes, session tokens.
- **`internal/webauth`, `internal/webmode`** — authentication on the HTTP and WebSocket
  surfaces, including anything reachable before authentication completes.
- **`internal/agent`** — sandbox and guardrail escapes: tool calls that reach outside the
  workspace, command execution that evades the egress guardrail, or prompt-injected
  content in a file causing unapproved actions.
- **Credential handling** — API keys leaking into logs, session transcripts, or error
  messages.

## Not in scope

- **The agent executing commands you approved.** That is the product working. Reports
  amounting to "the agent ran a command" without a bypass of a guardrail or approval
  step aren't vulnerabilities.
- **Exposing `tollecode web` on a public network without an access key.** The key is off
  by default because the default deployment is your own machine. Enabling it before you
  put the server on a network is your responsibility — see the README.
- **Vulnerabilities in AI provider APIs.** Report those to the provider.
- **Model behaviour** — hallucinations, bad code suggestions, or refusals.

## Deployment guidance

If you serve `tollecode web` beyond localhost, enable the access key first
(Settings → Security). Without it, anyone who can reach the port can run an agent with
your filesystem access and your API keys. Put it behind TLS as well; the access key is
sent by the browser on each connection and plain HTTP exposes it on the wire.
