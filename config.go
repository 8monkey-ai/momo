package main

import (
	"strings"

	"gopkg.in/ini.v1"
)

// The schema names no channel implementation; sections stay generic maps.
type config struct {
	Port     int
	Channels map[string]map[string]string
}

func loadConfig(path string) (config, error) {
	cfg := config{Channels: map[string]map[string]string{}}
	f, err := ini.Load(path)
	if err != nil {
		return cfg, err
	}
	cfg.Port = f.Section("").Key("port").MustInt(8080)
	for _, s := range f.Section("channels").ChildSections() {
		cfg.Channels[strings.TrimPrefix(s.Name(), "channels.")] = s.KeysHash()
	}
	return cfg, nil
}
