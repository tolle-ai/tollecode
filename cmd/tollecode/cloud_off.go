//go:build !cloud

package main

import "github.com/spf13/cobra"

// cloudCommands returns nothing in open-source builds: there is no managed cloud
// to run inside, so `cloud-session` is not registered. See cloud_on.go.
func cloudCommands() []*cobra.Command {
	return nil
}
