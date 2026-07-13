//go:build ignore

// The go:generate entry point for package ts0gen (see ts0gen.go for why
// this is an ignore-tagged stub instead of a normal main package). Run it
// by explicit file path — `go run ../../tools/ts0gen/main.go` from
// internal/server/dashboard, which is exactly what the directive in
// dashboard.go does; the go command ignores build constraints in
// explicitly named files, so the ignore tag only hides this file from
// ./... builds (and with it, from CI's matrix + autorelease).
package main

import (
	"fmt"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/tools/ts0gen"
)

func main() {
	if err := ts0gen.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ts0gen:", err)
		os.Exit(1)
	}
}
