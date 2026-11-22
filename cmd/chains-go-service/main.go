// chains-go-service launches the compiled-in Go service integrations.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"chains-automation/sdk"
	"chains-automation/services/keyboard"
	"chains-automation/services/onkyo"
)

var registry = map[string]func(context.Context, []string) error{
	"onkyo": func(ctx context.Context, args []string) error {
		settings := onkyo.DefaultSettings()
		config, err := sdk.LoadConfig("onkyo", args, &settings)
		if err != nil {
			return err
		}
		return sdk.RunService(ctx, config, onkyo.Definition(settings))
	},
	"keyboard": func(ctx context.Context, args []string) error {
		settings := keyboard.DefaultSettings()
		config, err := sdk.LoadConfig("keyboard", args, &settings)
		if err != nil {
			return err
		}
		return sdk.RunService(ctx, config, keyboard.Definition(settings))
	},
}

func run(ctx context.Context, args []string) error {
	if len(args) == 1 && args[0] == "--list" {
		names := make([]string, 0, len(registry))
		for name := range registry {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Println(name)
		}
		return nil
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, "Usage: chains-go-service <name> [options] or chains-go-service --list")
		return nil
	}
	start := registry[args[0]]
	if start == nil {
		return fmt.Errorf("unknown Go service %q", args[0])
	}
	return start(ctx, args[1:])
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
