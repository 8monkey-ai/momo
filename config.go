package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// config is the config.yaml schema. A channel is enabled by the presence of
// its section under channels; each section is kept as raw YAML for the
// channel's own config type to decode.
type config struct {
	Port     int                  `yaml:"port"`
	Channels map[string]yaml.Node `yaml:"channels"`
}

func loadConfig(path string) (config, error) {
	cfg := config{Port: 8080}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}
