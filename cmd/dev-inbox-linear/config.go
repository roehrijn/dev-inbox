package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/roehrijn/dev-inbox/internal/linearplugin"
)

// settings are the plugin's settings from its config file, with built-in defaults where it has none.
type settings struct {
	NATSURL       string // empty if unset
	LogLevel      string // empty if unset
	HorizonDays   int
	SweepInterval time.Duration
}

// fileConfig mirrors the config file; checkKeys keeps it in sync with knownKeys.
type fileConfig struct {
	NATSURL  string `yaml:"nats-url"`
	LogLevel string `yaml:"log-level"`
	// HorizonDays stays a node: decoding into an int would truncate 1.5.
	HorizonDays   yaml.Node `yaml:"horizon-days"`
	SweepInterval string    `yaml:"sweep-interval"`
}

var knownKeys = []string{"nats-url", "log-level", "horizon-days", "sweep-interval"}

func defaultSettings() settings {
	return settings{
		HorizonDays:   linearplugin.DefaultHorizonDays,
		SweepInterval: linearplugin.DefaultSweepInterval,
	}
}

// readSettings reads the config file at path. A missing file yields the defaults unless explicit is set.
func readSettings(path string, explicit bool) (settings, error) {
	s := defaultSettings()
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		return s, nil
	case errors.Is(err, fs.ErrNotExist):
		return s, usagef("config file %s does not exist", path)
	case err != nil:
		return s, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return s, usagef("invalid config %s: %w", path, err)
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		node = node.Content[0]
	}
	if node.Kind == 0 || node.Tag == "!!null" {
		return s, nil
	}
	if node.Kind != yaml.MappingNode {
		return s, usagef("invalid config %s: the document must be a mapping", path)
	}
	if err := checkKeys(node, path); err != nil {
		return s, err
	}
	var cfg fileConfig
	if err := node.Decode(&cfg); err != nil {
		return s, usagef("invalid config %s: %w", path, err)
	}
	if err := apply(&s, cfg); err != nil {
		return s, usagef("invalid config %s: %w", path, err)
	}
	return s, nil
}

// checkKeys rejects unknown top-level keys, which catches typos.
func checkKeys(node *yaml.Node, path string) error {
	var errs []error
	for i := 0; i+1 < len(node.Content); i += 2 {
		if key := node.Content[i].Value; !slices.Contains(knownKeys, key) {
			errs = append(errs, fmt.Errorf("invalid config %s: unknown key %q", path, key))
		}
	}
	if len(errs) > 0 {
		return usageError{errors.Join(errs...)}
	}
	return nil
}

// apply validates cfg and sets what it configures. Errors name the offending key.
func apply(s *settings, cfg fileConfig) error {
	if cfg.NATSURL != "" {
		s.NATSURL = cfg.NATSURL
	}
	s.LogLevel = cfg.LogLevel
	if n := &cfg.HorizonDays; n.Kind != 0 {
		var days int
		if n.Tag != "!!int" || n.Decode(&days) != nil {
			return fmt.Errorf("horizon-days: %q is not an integer", n.Value)
		}
		if days < 0 {
			return fmt.Errorf("horizon-days: %d is negative", days)
		}
		s.HorizonDays = days
	}
	if cfg.SweepInterval != "" {
		d, err := time.ParseDuration(cfg.SweepInterval)
		if err != nil {
			return fmt.Errorf("sweep-interval: %w", err)
		}
		if d <= 0 {
			return fmt.Errorf("sweep-interval: %s is not positive", cfg.SweepInterval)
		}
		s.SweepInterval = d
	}
	return nil
}
