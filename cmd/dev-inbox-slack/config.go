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

	"github.com/roehrijn/dev-inbox/internal/slackplugin"
)

// settings are the plugin's settings from its config file, with built-in defaults where it has none.
type settings struct {
	NATSURL      string // empty if unset
	LogLevel     string // empty if unset
	ClientID     string // empty if unset
	Channels     []string
	Groups       []string
	Score        int
	PollInterval time.Duration
	Lookback     time.Duration
}

// fileConfig mirrors the config file; checkKeys keeps it in sync with knownKeys.
type fileConfig struct {
	NATSURL  string   `yaml:"nats-url"`
	LogLevel string   `yaml:"log-level"`
	ClientID string   `yaml:"oauth-client-id"`
	Channels []string `yaml:"channels"`
	Groups   []string `yaml:"groups"`
	// Score stays a node: decoding into an int would truncate 1.5.
	Score        yaml.Node `yaml:"score"`
	PollInterval string    `yaml:"poll-interval"`
	Lookback     string    `yaml:"lookback"`
}

var knownKeys = []string{"nats-url", "log-level", "oauth-client-id", "channels", "groups", "score", "poll-interval", "lookback"}

func defaultSettings() settings {
	return settings{
		Score:        slackplugin.DefaultScore,
		PollInterval: slackplugin.DefaultPollInterval,
		Lookback:     slackplugin.DefaultLookback,
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

// apply validates cfg and sets what it configures. Errors name the offending key and never contain the
// client ID.
func apply(s *settings, cfg fileConfig) error {
	if cfg.NATSURL != "" {
		s.NATSURL = cfg.NATSURL
	}
	s.LogLevel = cfg.LogLevel
	s.ClientID = strings.TrimSpace(cfg.ClientID)
	for _, c := range cfg.Channels {
		c = strings.TrimSpace(c)
		if c == "" || c == "#" {
			return errors.New("channels: empty channel")
		}
		s.Channels = append(s.Channels, c)
	}
	for _, g := range cfg.Groups {
		if g == "" || strings.ContainsAny(g, " \t\n") {
			return fmt.Errorf("groups: %q is not a user group ID", g)
		}
	}
	s.Groups = cfg.Groups
	if n := &cfg.Score; n.Kind != 0 {
		var score int
		if n.Tag != "!!int" || n.Decode(&score) != nil {
			return fmt.Errorf("score: %q is not an integer", n.Value)
		}
		if score <= 0 {
			return fmt.Errorf("score: %d is not positive", score)
		}
		s.Score = score
	}
	var err error
	if s.PollInterval, err = duration("poll-interval", cfg.PollInterval, s.PollInterval); err != nil {
		return err
	}
	if s.Lookback, err = duration("lookback", cfg.Lookback, s.Lookback); err != nil {
		return err
	}
	return nil
}

// duration parses the positive duration text of key, or returns def if text is empty.
func duration(key, text string, def time.Duration) (time.Duration, error) {
	if text == "" {
		return def, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: %s is not positive", key, text)
	}
	return d, nil
}
