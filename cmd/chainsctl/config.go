package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"chains-automation/internal/tokencommand"
	"github.com/pelletier/go-toml/v2"
)

type clientConfig struct {
	URL          string   `toml:"url"`
	Token        *string  `toml:"token"`
	TokenCommand []string `toml:"token_command"`
}

func operatorConnection(ctx context.Context, configPath, flagURL string) (url, token string, err error) {
	if env := os.Getenv("CHAINS_OPERATOR_CONFIG"); env != "" {
		configPath = env
	}
	settings := clientConfig{URL: "http://127.0.0.1:8080"}
	explicit := configPath != ""
	if !explicit {
		if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
			configPath = filepath.Join(dir, "chains", "chainsctl.toml")
		} else if home, err := os.UserHomeDir(); err == nil {
			configPath = filepath.Join(home, ".config", "chains", "chainsctl.toml")
		}
	}
	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			if err := toml.NewDecoder(bytes.NewReader(raw)).DisallowUnknownFields().Decode(&settings); err != nil {
				return "", "", fmt.Errorf("%s: invalid TOML configuration (expected url, token or token_command)", configPath)
			}
		case explicit || !errors.Is(err, os.ErrNotExist):
			return "", "", err
		}
	}
	url = settings.URL
	if flagURL != "" {
		url = flagURL
	}
	if env, ok := os.LookupEnv("CHAINS_OPERATOR_URL"); ok {
		url = env
	}
	if settings.Token != nil && settings.TokenCommand != nil {
		return "", "", errors.New("configure either token or token_command")
	}
	if token, ok := os.LookupEnv("CHAINS_OPERATOR_TOKEN"); ok {
		return url, token, nil
	}
	if settings.Token == nil && settings.TokenCommand == nil {
		return url, "", nil
	}
	if strings.TrimRight(url, "/") != strings.TrimRight(settings.URL, "/") {
		return "", "", errors.New("operator URL differs from the configured endpoint, set CHAINS_OPERATOR_TOKEN or use a config for that endpoint")
	}
	if settings.Token != nil {
		return url, *settings.Token, nil
	}
	token, err = tokencommand.Read(ctx, settings.TokenCommand)
	if err != nil {
		return "", "", fmt.Errorf("token_command: %w", err)
	}
	return url, token, nil
}
