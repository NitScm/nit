package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NitScm/nit/internal/bootstrap"
)

// configCommand inspects and creates the configuration file.
//
// `config show` is the answer to "why is this setting what it is?" — the
// question an operator asks at the worst possible moment, whose honest answer
// otherwise needs three places checked: the defaults, the file and the
// environment.
func configCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nitctl config show|init|path")
	}

	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	configFile := fs.String("config", "", "configuration file to read or write")
	force := fs.Bool("force", false, "overwrite an existing file (init)")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	switch args[0] {
	case "show":
		return configShow(*configFile)
	case "path":
		return configPath(*configFile)
	case "init":
		return configInit(*configFile, *force)
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

// configShow prints the effective configuration and where each value came from.
func configShow(path string) error {
	cfg, err := bootstrap.LoadConfigFrom(path)

	// A configuration that will not start is exactly when this command is most
	// useful, so print what could be assembled before reporting the problem.
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration is not usable: %v\n\n", err)
	}

	if cfg.ConfigFile != "" {
		fmt.Printf("file: %s\n\n", cfg.ConfigFile)
	} else {
		fmt.Printf("file: none found (environment and defaults only)\n\n")
	}

	fmt.Println(cfg.RedactedHeader())

	for _, row := range cfg.Redacted() {
		fmt.Println(row)
	}

	if err != nil {
		return err
	}

	return nil
}

// configPath reports which file would be read, without reading it.
func configPath(explicit string) error {
	path, err := bootstrap.FindConfigFile(explicit)
	if err != nil {
		return err
	}

	if path == "" {
		fmt.Println("no configuration file found; searched:")
		for _, candidate := range bootstrap.SearchPaths() {
			fmt.Printf("  %s\n", candidate)
		}
		return nil
	}

	fmt.Println(path)

	return nil
}

// configInit writes a commented configuration file.
//
// Mode 600 from the start: the file is where a database password and a signing
// key end up, and a file created readable and tightened later has a window in
// which it was not.
func configInit(path string, force bool) error {
	if path == "" {
		path = bootstrap.DefaultConfigPath
	}

	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; pass -force to overwrite", path)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	if err := os.WriteFile(path, []byte(bootstrap.ExampleFile), 0o600); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n\n", path)
	fmt.Println("Next:")
	fmt.Printf("  openssl rand -base64 32 > %s\n", filepath.Join(filepath.Dir(path), "sync.key"))
	fmt.Printf("  chmod 600 %s\n", filepath.Join(filepath.Dir(path), "sync.key"))
	fmt.Println("  edit database.url and policy.dir, then: nitctl config show")

	return nil
}
