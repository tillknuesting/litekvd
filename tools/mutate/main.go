// Command mutate breaks the server on purpose, one change at a time, and sees
// whether a test notices.
//
//	go run ./tools/mutate                 # all of them
//	go run ./tools/mutate replica batch   # only those whose name matches
//
// The machinery is in github.com/tillknuesting/litekv/mutate, which this
// repository already depends on for the store; what is here is its own list of
// what to break, in mutations.go, and the one setting that belongs to this
// suite rather than to the tool.
//
// Every mutation must be caught except the five listed in AGENTS.md with the
// reason for each. A sixth survivor is news: it means something this code
// promises has no test holding it there.
package main

import (
	"fmt"
	"os"

	"github.com/tillknuesting/litekv/mutate"
)

// This suite takes about six seconds under -race, so ninety is room to spare
// and a saving of eight and a half minutes on the one mutation that wedges
// something. See mutate.Options.Timeout for why erring the other way is the
// worst thing that can be done to this tool.
const timeout = "90s"

func main() {
	if err := mutate.Run(mutations, mutate.Options{Timeout: timeout}, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mutate:", err)
		os.Exit(1)
	}
}
