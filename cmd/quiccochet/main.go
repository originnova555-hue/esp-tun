// Command quiccochet is the tunnel engine: a persistent layer-3 TUN interface
// carried over QUIC datagrams on a source-spoofed UDP socket.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/originnova555-hue/esp-tun/internal/config"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `quiccochet — layer-3 anti-censorship tunnel

Usage:
  quiccochet run     -config <file>        Run a tunnel instance
  quiccochet check   -config <file>        Parse and validate a config, then exit
  quiccochet sample  [-tier T] [-role R]   Print a starter config
  quiccochet genpsk                        Print a fresh pre-shared key
  quiccochet cpuinfo                       Show which AEAD this CPU will pick
  quiccochet admin   <command> [flags]     Talk to a running instance
  quiccochet version                       Print the version

Tiers: light | medium | high | ultra    Roles: server | client
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "sample":
		err = cmdSample(os.Args[2:])
	case "genpsk":
		err = cmdGenPSK()
	case "cpuinfo":
		err = cmdCPUInfo()
	case "admin":
		err = cmdAdmin(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdSample(args []string) error {
	fs := flag.NewFlagSet("sample", flag.ExitOnError)
	tier := fs.String("tier", config.TierMedium, "tier preset: light|medium|high|ultra")
	role := fs.String("role", string(config.RoleServer), "role: server|client")
	name := fs.String("name", "", "tunnel name")
	out := fs.String("out", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := config.Sample(config.SampleOptions{
		Tier: strings.ToLower(*tier),
		Role: config.Role(strings.ToLower(*role)),
		Name: *name,
	})
	if err != nil {
		return err
	}
	if *out != "" {
		return os.WriteFile(*out, []byte(s), 0o600)
	}
	fmt.Print(s)
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := fs.String("config", "", "path to the config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("-config is required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	for _, w := range config.Warnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	fmt.Printf("ok: %s (%s, tier %s) — %s over %s, pool %d, obfuscation %s\n",
		cfg.Name, cfg.Role, cfg.Tier, cfg.TUN.Name, cfg.Transport.Listen,
		cfg.QUIC.PoolSize, cfg.Obfs.Mode)
	return nil
}

func cmdGenPSK() error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	fmt.Println(base64.RawURLEncoding.EncodeToString(b))
	return nil
}
