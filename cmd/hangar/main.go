// Command hangar is the Hangar CLI; see internal/cli.
package main

import (
	"context"
	"os"

	"github.com/YukinaMochizuki/hangar/internal/cli"
)

func main() {
	// cli.Run takes the signals itself: hangar exec passes SIGINT on to the
	// remote process instead of ending.
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Env: os.Getenv}, cli.Options{}))
}
