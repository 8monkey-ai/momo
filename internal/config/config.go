// Package config reads momo's configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/channel"
)

// DefaultListen is the address momo serves on when the file says nothing.
const DefaultListen = ":8080"

// Config is the configuration momo runs with. Channel blocks stay undecoded so
// that each channel owns its own settings.
type Config struct {
	Listen   string
	Channels map[string]channel.Decoder
}

type file struct {
	Listen   string               `yaml:"listen"`
	Channels map[string]yaml.Node `yaml:"channels"`
}

// Load reads the configuration file at path and applies defaults.
func Load(path string) (*Config, error) {
	// The operator chooses this path with the -config flag; reading it is the point.
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, err
	}
	return parse(raw)
}

func parse(raw []byte) (*Config, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	// An empty file is a valid configuration: everything takes its default.
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	// Decode reads one document per call, so a second document would sit in the
	// file taking effect on nothing.
	var rest yaml.Node
	if err := dec.Decode(&rest); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid configuration: unexpected content after the first YAML document")
	}
	cfg := &Config{Listen: f.Listen, Channels: map[string]channel.Decoder{}}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	for name, node := range f.Channels {
		cfg.Channels[name] = decoderFor(node)
	}
	return cfg, nil
}

func decoderFor(node yaml.Node) channel.Decoder {
	return func(v any) error {
		if node.IsZero() {
			return nil
		}
		// yaml.Node.Decode does not inherit KnownFields, so the block is decoded
		// through a strict decoder of its own: a misspelled channel setting is an
		// error here too, not a silently kept default.
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
}
