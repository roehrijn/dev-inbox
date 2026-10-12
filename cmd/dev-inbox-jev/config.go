package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/roehrijn/dev-inbox/internal/jevplugin"
)

// settings are the plugin's settings from its config file, with built-in defaults where it has none.
type settings struct {
	NATSURL   string // empty if unset
	LogLevel  string // empty if unset
	Threshold float64
	Weights   map[string]int
	Repos     []string
	Debounce  time.Duration
}

// fileConfig mirrors the config file; checkKeys keeps it in sync with knownKeys.
type fileConfig struct {
	NATSURL   string   `yaml:"nats-url"`
	LogLevel  string   `yaml:"log-level"`
	Threshold *float64 `yaml:"threshold"`
	// Weights stay nodes: decoding into ints would truncate 1.5.
	Weights  map[string]yaml.Node `yaml:"weights"`
	Repos    []string             `yaml:"repos"`
	Debounce string               `yaml:"debounce"`
}

var knownKeys = []string{"nats-url", "log-level", "threshold", "weights", "repos", "debounce"}

// weightKey is the config key of an intent's weight, e.g. question-for-me for question_for_me.
func weightKey(intent string) string { return strings.ReplaceAll(intent, "_", "-") }

func defaultSettings() settings {
	s := settings{
		Threshold: jevplugin.DefaultThreshold,
		Weights:   make(map[string]int, len(jevplugin.DefaultWeights)),
		Debounce:  jevplugin.DefaultDebounce,
	}
	for k, v := range jevplugin.DefaultWeights {
		s.Weights[k] = v
	}
	return s
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

// checkKeys rejects unknown top-level keys and unknown keys of weights, which catches typos.
func checkKeys(node *yaml.Node, path string) error {
	var errs []error
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if !slices.Contains(knownKeys, key.Value) {
			errs = append(errs, fmt.Errorf("invalid config %s: unknown key %q", path, key.Value))
			continue
		}
		if key.Value != "weights" || val.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(val.Content); j += 2 {
			w := val.Content[j].Value
			if !slices.ContainsFunc(jevplugin.Intents, func(in string) bool { return weightKey(in) == w }) {
				errs = append(errs, fmt.Errorf("invalid config %s: unknown key %q", path, "weights."+w))
			}
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
	if t := cfg.Threshold; t != nil {
		if *t < 0 || *t > 1 {
			return fmt.Errorf("threshold: %v is not between 0 and 1", *t)
		}
		s.Threshold = *t
	}
	for _, in := range jevplugin.Intents {
		node, ok := cfg.Weights[weightKey(in)]
		if !ok {
			continue
		}
		var w int
		if node.Tag != "!!int" || node.Decode(&w) != nil {
			return fmt.Errorf("weights.%s: %q is not an integer", weightKey(in), node.Value)
		}
		s.Weights[in] = w
	}
	for _, r := range cfg.Repos {
		if err := validateRepo(r); err != nil {
			return fmt.Errorf("repos: %w", err)
		}
	}
	s.Repos = cfg.Repos
	if cfg.Debounce != "" {
		d, err := time.ParseDuration(cfg.Debounce)
		if err != nil {
			return fmt.Errorf("debounce: %w", err)
		}
		if d < 0 {
			return fmt.Errorf("debounce: %s is negative", cfg.Debounce)
		}
		s.Debounce = d
	}
	return nil
}

// validateRepo checks that r has the form org or org/repo.
func validateRepo(r string) error {
	org, repo, hasRepo := strings.Cut(r, "/")
	if org == "" || hasRepo && (repo == "" || strings.Contains(repo, "/")) || strings.ContainsAny(r, " \t\n") {
		return fmt.Errorf("%q is not of the form org or org/repo", r)
	}
	return nil
}
