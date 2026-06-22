// Command flynn is the standalone Flynn agent binary.
//
// Build:  go build -o flynn ./cmd/flynn
// Run:    ./flynn --model anthropic:claude-opus-4-8
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	agent "github.com/ionalpha/flynn"
	"github.com/ionalpha/flynn/internal/version"
)

func main() {
	var (
		model       = flag.String("model", "", "model id as provider:model")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	a := agent.New(agent.Config{
		Model: *model,
		Out:   os.Stdout,
	})

	if err := a.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
