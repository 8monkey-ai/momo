package main

import (
	"fmt"
	"os"

	"github.com/8monkey-ai/momo/channel/respondio"
	"gopkg.in/yaml.v3"
)

// config is the config.yaml schema. A channel is enabled by the presence of
// its section under channels.
type config struct {
	Port     int `yaml:"port"`
	Channels struct {
		Respondio *respondio.Config `yaml:"respondio"`
	} `yaml:"channels"`
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
