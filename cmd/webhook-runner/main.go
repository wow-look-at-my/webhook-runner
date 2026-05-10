// Command webhook-runner is the binary entry point.
//
// All command logic lives in internal/cli; main exists only to call into
// that package. See README.md for usage.
package main

import "github.com/wow-look-at-my/webhook-runner/internal/cli"

func main() {
	cli.Execute()
}
