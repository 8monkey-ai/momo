package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Channels stays raw YAML so the schema names no channel implementation;
// main decodes each section into its channel's config type.
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
