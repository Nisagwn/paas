// Command paas-dockerfile prints the Dockerfile the platform would generate
// for a project directory, so it can be inspected or built locally:
//
//	go run ./cmd/paas-dockerfile ./examples/node-hello
package main

import (
	"fmt"
	"os"

	"github.com/nisagwn/paas/internal/build"
)

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	plan, err := build.Detect(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "framework: %s\ndetected: %s\n", plan.Framework, plan.Summary)
	if plan.Dockerfile == "" {
		fmt.Fprintln(os.Stderr, "the repository brings its own Dockerfile; it is used as is")
		return
	}
	fmt.Print(plan.Dockerfile)
}
