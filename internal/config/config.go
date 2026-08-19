// Package config reads momo's configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/8monkey-ai/momo/internal/channel"
	"github.com/8monkey-ai/momo/internal/core"
)

// Config is the configuration momo runs with. The channel blocks and the agent
// block stay undecoded so that each of them owns its own settings.
type Config struct {
	Listen            string
	MaxConnections    int
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	Delivery          core.Pacing
	Channels          map[string]channel.Decoder
	Agent             func(any) error
}

type file struct {
	Listen            string               `yaml:"listen"`
	MaxConnections    int                  `yaml:"max_connections"`
	ReadHeaderTimeout *time.Duration       `yaml:"read_header_timeout"`
	ReadTimeout       *time.Duration       `yaml:"read_timeout"`
	IdleTimeout       *time.Duration       `yaml:"idle_timeout"`
	ShutdownTimeout   *time.Duration       `yaml:"shutdown_timeout"`
	Delivery          delivery             `yaml:"delivery"`
	Channels          map[string]yaml.Node `yaml:"channels"`
	Agent             yaml.Node            `yaml:"agent"`
}

// delivery is the pacing block. The durations are pointers so that an absent
// setting takes its default and a value the operator set is checked.
type delivery struct {
	DelayPerWord *time.Duration `yaml:"delay_per_word"`
	MaxDelay     *time.Duration `yaml:"max_delay"`
	Separator    *string        `yaml:"separator"`
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
	pace, err := pacing(f.Delivery)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Listen:            f.Listen,
		MaxConnections:    f.MaxConnections,
		ReadHeaderTimeout: duration(f.ReadHeaderTimeout, 10*time.Second),
		// net/http clears the read deadline before the handler runs, so this bounds a
		// request's body without touching a stream the handler keeps open.
		ReadTimeout:     duration(f.ReadTimeout, 30*time.Second),
		IdleTimeout:     duration(f.IdleTimeout, 2*time.Minute),
		ShutdownTimeout: duration(f.ShutdownTimeout, 20*time.Second),
		Delivery:        pace,
		Channels:        map[string]channel.Decoder{},
		Agent:           decoderFor(f.Agent),
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 1024
	}
	// A negative limit reaches the listener as a channel of negative capacity.
	if cfg.MaxConnections < 0 {
		return nil, errors.New("invalid configuration: max_connections cannot be negative")
	}
	for name, node := range f.Channels {
		cfg.Channels[name] = decoderFor(node)
	}
	return cfg, nil
}

// pacing reads the delivery block. A pause momo cannot wait and a separator that
// would close a paragraph at every character are refused before the process
// serves anything.
func pacing(d delivery) (core.Pacing, error) {
	p := core.Pacing{
		DelayPerWord: duration(d.DelayPerWord, time.Second),
		MaxDelay:     duration(d.MaxDelay, 10*time.Minute),
		Separator:    "\n\n",
	}
	if p.DelayPerWord < 0 {
		return core.Pacing{}, errors.New("invalid configuration: delivery.delay_per_word cannot be negative")
	}
	if p.MaxDelay <= 0 {
		return core.Pacing{}, errors.New("invalid configuration: delivery.max_delay must be positive")
	}
	if d.Separator != nil {
		if *d.Separator == "" {
			return core.Pacing{}, errors.New("invalid configuration: delivery.separator cannot be empty")
		}
		p.Separator = *d.Separator
	}
	return p, nil
}

func duration(set *time.Duration, fallback time.Duration) time.Duration {
	if set == nil {
		return fallback
	}
	return *set
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
