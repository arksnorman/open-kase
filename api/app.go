// Command open-kase is the single binary for the API server, background
// workers and operator CLI. It only handles signals and exit codes; all
// behaviour lives in the runtime package.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	appruntime "github.com/arksnorman/open-kase/api/runtime"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := appruntime.Execute(ctx, os.Args[1:], appruntime.IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr})
	stop()
	os.Exit(code)
}
