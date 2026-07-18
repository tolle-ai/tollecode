# TolleCode CLI

An agentic AI coding assistant for the terminal. Multi-provider by design — bring
your own key, or run entirely locally against Ollama.

```
go install github.com/tolle-ai/tollecode/cmd/tollecode@latest
```

## Providers

TolleCode is not tied to one vendor. Every provider implements the same
`ai.Provider` interface (`internal/ai/types.go`):

| Provider  | Notes                                                    |
|-----------|----------------------------------------------------------|
| Anthropic | Claude models                                            |
| OpenAI    | GPT models, plus any OpenAI-compatible endpoint          |
| Ollama    | Local models — no API key, nothing leaves your machine   |

```sh
tollecode configure set-key anthropic sk-...
tollecode launch ollama --model qwen2.5-coder   # fully local
```

## Usage

```sh
tollecode                       # interactive REPL in the current directory
tollecode --task "add tests"    # one-shot, non-interactive
tollecode sessions list         # browse past sessions
tollecode web                   # serve the browser UI
tollecode serve                 # run as an HTTP/WebSocket API
```

## Web mode and the access key

`tollecode web` serves a browser UI. It binds locally by default, but if you put it
behind nginx on a public host, **turn on the access key first** — otherwise anyone
who can reach the port can drive an agent on your machine.

```sh
tollecode web            # prints the access key when one is required
```

Generate or rotate the key from Settings → Security. Rotating immediately
invalidates every browser still holding the old key. The key is stored on your
machine, is compared in constant time, and grants expire after 30 days
(`internal/liteaccess`). This is your gate, not ours — there is no phone-home.

## Building from source

```sh
git clone https://github.com/tolle-ai/tollecode
cd tollecode
go build ./cmd/tollecode
go test ./...
```

Requires the Go version in `go.mod`.

**Note on `tollecode web` in source builds:** the browser UI is compiled in via
`go:embed` from `internal/webmode/dist/browser`, which ships a placeholder here.
Source builds serve no UI until a build is staged there; the released binaries
include it. The CLI, `serve`, and every provider work normally either way.

## What's in this repository

The agent loop, tool execution, provider adapters, session storage, MCP and LSP
integration, the local web server, and the CLI itself.

Not included: the hosted cloud service and the self-hosted team server are separate
products and are not open source. The CLI does not depend on either — it is fully
functional standalone with your own provider keys.

## License

Copyright (c) 2026 Tollecode. Licensed under the
[GNU Affero General Public License v3.0](LICENSE).

AGPL section 13 matters here: if you modify TolleCode and let others use it over a
network, you must offer those users the modified source. Running it unmodified, or
modifying it for your own use, carries no such obligation.

## Contributing

**We're not accepting pull requests yet.** The project isn't set up to take outside
code at the moment, so PRs will be closed unmerged — please don't spend time on one.
This will change; watch this section.

**Bug reports and feature requests are welcome now.** Open an issue.

See [CONTRIBUTING.md](CONTRIBUTING.md) for how the codebase is laid out and how to
build it, and [CLA.md](CLA.md) for the agreement that will apply once contributions
open.

Found a security issue? Please don't open a public issue — see
[SECURITY.md](SECURITY.md).
