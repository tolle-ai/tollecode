//go:build !selfhost

// Package selfhostgate is the single seam between the open-source CLI and the
// closed-source selfhost server. See gate_on.go for the full contract.
//
// This is the open-source build: selfhost is not compiled in, so TryServe always
// declines and the caller falls through to the community API. This file must not
// import internal/selfhost — it is the reason the public tree builds without it.
package selfhostgate

import (
	"context"

	"github.com/tolle-ai/tollecode/internal/httpserver"
)

// Available reports whether selfhost support was compiled into this binary.
const Available = false

// TryServe always returns false in open-source builds: there is no selfhost
// server to hand off to, so the caller starts the community API as usual.
func TryServe(_ context.Context, _ httpserver.ServerConfig, _ bool) (bool, error) {
	return false, nil
}
