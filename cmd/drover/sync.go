package main

import (
	"flag"
	"fmt"
	"os"
)

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		dataDirFlag = fs.String("data-dir", "", "")
		configFlag  = fs.String("config", "", "")
		serverFlag  = fs.String("server", "", "")
	)
	flags, rest := splitArgs(args, clientFlags())
	if err := fs.Parse(flags); err != nil {
		return err
	}

	c, err := clientFor(dataDirFlag, configFlag, serverFlag)
	if err != nil {
		return err
	}

	// A refresh is queued, not awaited: a big clone can take minutes, and
	// blocking the CLI on it would be worse than letting `get` report the
	// phase.
	if len(rest) == 0 {
		if err := c.SyncAll(); err != nil {
			return err
		}
		fmt.Println("refresh queued for every repository; watch it with `drover get repository`")
		return nil
	}
	if err := c.Sync(rest[0]); err != nil {
		return err
	}
	fmt.Printf("refresh queued for %s\n", rest[0])
	return nil
}
