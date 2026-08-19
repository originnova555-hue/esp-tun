package main

import (
	"flag"
	"fmt"
)

// cmdRun is filled in once the datapath lands; keeping the entry point here
// means the CLI surface stays stable while the engine grows underneath it.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", "", "path to the config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("-config is required")
	}
	return runTunnel(*path)
}
