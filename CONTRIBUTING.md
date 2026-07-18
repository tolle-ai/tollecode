# Contributing to TolleCode

> ## ⚠️ We are not accepting pull requests yet
>
> TolleCode is open source, but the project isn't set up to take outside code at the
> moment. **Pull requests will be closed unmerged** — please don't spend time on one.
>
> **Bug reports and feature requests are very welcome.** Open an issue: they cost you
> nothing to file, they're genuinely useful to us, and they carry none of the
> licensing questions that code does.
>
> When contributions do open up, it will be announced in the README and this notice
> will be replaced by the process below.

## What's coming when contributions open

TolleCode is open source under the AGPL, and it is also the foundation of commercial
products. To keep both possible, contributors will sign a Contributor License
Agreement granting Tollecode the right to license contributions under terms other
than the AGPL. See [CLA.md](CLA.md) — it's published now so you can read it in advance
and decide whether it works for you.

The short version: you keep the copyright in your work, and we can also ship it in
commercial builds. Nothing to sign today.

The rest of this document describes the codebase, and applies whether or not you're
sending patches — it's just as useful for reading the code or building your own fork.

## Development

```sh
git clone https://github.com/tolle-ai/tollecode
cd tollecode
go build ./cmd/tollecode
go test ./...
```

Requires the Go version pinned in `go.mod`. There are no code-generation steps and no
external services needed for the test suite.

If you're working in a fork, these are the checks the project holds itself to:

```sh
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

CI builds and tests on Linux and macOS; `gofmt` and `go vet` are on you.

Windows is a supported target but is **not** covered by CI, so nothing catches a
Windows regression automatically. The CLI does terminal, PTY, and filesystem work that
diverges sharply across platforms — please avoid POSIX-only assumptions in
`internal/agent` and `internal/shellenv`, and if you touch those, say in your report
whether you were able to check Windows.

## Repository layout

```
cmd/tollecode/     CLI entrypoint (cobra commands)
main.go            sidecar entrypoint — stdio IPC and HTTP server modes
internal/
  ai/              provider adapters — Anthropic, OpenAI-compatible, Ollama
  agent/           agentic loop: executor, tool dispatch, browser tools
  cli/             interactive REPL
  session/         session persistence and the live event bus
  httpserver/      HTTP + WebSocket API
  stdio/           JSON-over-stdio server (desktop IPC)
  webmode/         browser UI server
  liteaccess/      web-mode access key ("the door")
  mcp/, lsp/       MCP client and language-server integration
```

## Working on providers

TolleCode is deliberately not tied to one vendor. Anthropic, OpenAI, and Ollama all
implement the `ai.Provider` interface in `internal/ai/types.go`.

If you touch `internal/ai/`, `internal/agent/`, or anything handling `ai.ToolResult`,
`ai.ChatMessage`, or `ai.StreamRequest`, **consider all three providers**, not just the
one you use. Provider-specific behaviour belongs behind the interface, not in the agent
loop. Adding a fourth provider is a great first contribution — implement the interface
and register it in `internal/ai/manager.go`.

## About `internal/selfhostgate`

You'll find `internal/selfhostgate/gate_off.go`, whose `TryServe` always returns
`false`. That is not dead code or an unfinished feature. It's a build-tag seam: the
commercial self-host server compiles a tagged counterpart in its place. Please leave
the signature alone — changing it breaks a build you can't see from here. Everything
else in the repository is the real implementation.

## Reporting bugs

Include your OS, `tollecode` version, the provider and model, and the exact command.
Redact API keys from any logs you paste — the CLI does not scrub them for you.

For security issues, please do **not** open a public issue. See
[SECURITY.md](SECURITY.md).
