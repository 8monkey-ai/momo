package main

import (
	"strings"

	"gopkg.in/ini.v1"
)

// Channels stays raw INI sections so the schema names no channel
// implementation; main reads each section into its channel's config type.
type config struct {
	Port     int
	Channels map[string]*ini.Section
}

func loadConfig(path string) (config, error) {
	cfg := config{Channels: map[string]*ini.Section{}}
	f, err := ini.Load(path)
	if err != nil {
		return cfg, err
	}
	cfg.Port = f.Section("").Key("port").MustInt(8080)
	for _, s := range f.Section("channels").ChildSections() {
		cfg.Channels[strings.TrimPrefix(s.Name(), "channels.")] = s
	}
	return cfg, nil
}
