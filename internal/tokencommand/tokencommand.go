// Package tokencommand reads a bearer token from a configured executable.
package tokencommand

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

const maxOutput = 4096

type output struct{ strings.Builder }

func (o *output) Write(p []byte) (int, error) {
	if len(p) > maxOutput-o.Len() {
		return 0, errors.New("output too large")
	}
	return o.Builder.Write(p)
}

func Read(ctx context.Context, argv []string) (string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return "", errors.New("token command requires an executable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var stdout output
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Execution errors and stderr can contain command arguments or secrets.
		return "", errors.New("token command failed or exceeded its 4 KiB output limit")
	}
	token := strings.TrimSuffix(stdout.String(), "\n")
	if len(token) != stdout.Len() {
		token = strings.TrimSuffix(token, "\r")
	}
	if token == "" || strings.IndexFunc(token, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", errors.New("token command must print one nonempty bearer token")
	}
	return token, nil
}
