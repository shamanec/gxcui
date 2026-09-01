// Command gxcui runs XCUITests in parallel across booted simulators.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := newRootCommand().ExecuteContext(ctx)
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "gxcui: interrupted")
	case errors.Is(err, errTestsFailed):
		// The summary has already said which tests failed, in more detail than
		// an error line could.
	case err != nil:
		// `run` silences cobra's own error printing so that the summary is the
		// last thing on screen; without this, a run that could not start would
		// exit 2 saying nothing at all.
		fmt.Fprintf(os.Stderr, "gxcui: %v\n", err)
	}
	os.Exit(exitCodeFor(err))
}
