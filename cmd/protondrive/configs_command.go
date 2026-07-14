package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ColinMario/Protondrive-for-Linux/internal/customconfigs"
	"github.com/ColinMario/Protondrive-for-Linux/internal/safefile"
)

func runConfigs(remote string, args []string) error {
	fs := flag.NewFlagSet("configs", flag.ContinueOnError)
	force := fs.Bool("force", false, "Allow overwriting an existing file when using 'init'")
	if err := parseCommandFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	forceSet := false
	fs.Visit(func(current *flag.Flag) {
		if current.Name == "force" {
			forceSet = true
		}
	})

	remaining := fs.Args()
	if len(remaining) == 0 {
		if forceSet {
			return errors.New("--force is only valid with 'configs init'")
		}
		return printSyncConfigList()
	}
	if forceSet && remaining[0] != "init" {
		return errors.New("--force is only valid with 'configs init'")
	}

	switch remaining[0] {
	case "list":
		if len(remaining) != 1 {
			return errors.New("usage: protondrive configs list")
		}
		return printSyncConfigList()
	case "show":
		if len(remaining) != 2 {
			return errors.New("usage: protondrive configs show <name-or-path>")
		}
		return showSyncConfig(remaining[1])
	case "init":
		if len(remaining) != 2 {
			return errors.New("usage: protondrive configs init <template-name>")
		}
		dest, err := writeBuiltinConfig(remaining[1], *force)
		if err != nil {
			return err
		}
		fmt.Printf("Template copied to %s\n", dest)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (expected list, show, init)", remaining[0])
	}
}

func printSyncConfigList() error {
	builtins, err := customconfigs.List()
	if err != nil {
		return fmt.Errorf("unable to list built-in templates: %w", err)
	}
	fmt.Println("Built-in templates:")
	if len(builtins) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, tpl := range builtins {
			fmt.Printf("  - %s (%s)\n", tpl.ID, tpl.Description)
		}
	}
	fmt.Println()

	customConfigs, dir, err := listCustomSyncConfigs()
	if err != nil {
		return fmt.Errorf("unable to list custom configs: %w", err)
	}
	fmt.Printf("Custom config directory: %s\n", dir)
	if len(customConfigs) == 0 {
		fmt.Println("  (no JSON configs found yet)")
	} else {
		for _, summary := range customConfigs {
			fmt.Printf("  - %s (%s)\n", summary.Name, summary.Description)
			fmt.Printf("    %s\n", summary.File)
		}
	}
	fmt.Println("\nUse 'protondrive configs init <template>' to copy a built-in template, then edit it to match your paths.")
	return nil
}

func showSyncConfig(identifier string) error {
	cfg, err := loadSyncConfig(identifier)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg.Config, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	fmt.Printf("\nSource: %s\n", describeConfigSource(cfg.Source))
	return nil
}

func writeBuiltinConfig(name string, force bool) (string, error) {
	template, found, err := customconfigs.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("unable to load built-in templates: %w", err)
	}
	if !found {
		return "", fmt.Errorf("built-in template %q not found; run 'protondrive configs list' for options", name)
	}
	if _, err := parseSyncConfig(template.Raw); err != nil {
		return "", fmt.Errorf("built-in template %q is invalid: %w", template.Name, err)
	}
	dir, err := ensureSyncConfigDir()
	if err != nil {
		return "", err
	}
	filename := configFileName(template.ID)
	dest := filepath.Join(dir, filename)
	if !force {
		if _, err := os.Stat(dest); err == nil {
			return "", fmt.Errorf("%s already exists (re-run with --force to overwrite)", dest)
		}
	}
	if err := safefile.Write(dest, template.Raw, 0o600); err != nil {
		return "", err
	}
	return dest, nil
}
