package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"
)

// pluginsKey is the top-level key of the plugin namespace, which checkKeys leaves alone.
const pluginsKey = "plugins"

// section is a command whose flags are also settable under the key of the same name in the config file.
// The flags double as the schema of the section: their names are its keys and their defaults its defaults.
type section struct {
	name   string
	define func(*pflag.FlagSet)
}

var (
	serveSection = section{"serve", defineServeFlags}
	tuiSection   = section{"tui", defineTUIFlags}
	sections     = []section{serveSection, tuiSection}
)

// flags returns the section's flags in definition order, carrying their built-in defaults.
func (s section) flags() []*pflag.Flag {
	set := pflag.NewFlagSet(s.name, pflag.ContinueOnError)
	s.define(set)
	var flags []*pflag.Flag
	set.VisitAll(func(f *pflag.Flag) { flags = append(flags, f) })
	return flags
}

// settings mirrors the config file. UnmarshalExact fails when a flag has no field, so the two cannot drift apart.
// Durations stay strings so that a bare number is rejected instead of read as nanoseconds.
type settings struct {
	Serve struct {
		StateDir          string   `mapstructure:"state-dir"`
		NATSHost          string   `mapstructure:"nats-host"`
		NATSPort          int      `mapstructure:"nats-port"`
		PollInterval      string   `mapstructure:"poll-interval"`
		ReconcileInterval string   `mapstructure:"reconcile-interval"`
		LogLevel          string   `mapstructure:"log-level"`
		Greylist          []string `mapstructure:"greylist"`
		// PreferredTeams stays raw like Score: preferredTeams rejects 1.5 or "20" instead of coercing them.
		PreferredTeams map[string]any `mapstructure:"preferred-teams"`
		// Score stays raw: decoding into ints would coerce 1.5 or "20", which scoreWeights rejects.
		Score map[string]any `mapstructure:"score"`
	} `mapstructure:"serve"`
	TUI struct {
		NATSURL string `mapstructure:"nats-url"`
	} `mapstructure:"tui"`
	// Plugins exists so that UnmarshalExact accepts the namespace; the core never reads or validates it.
	Plugins map[string]any `mapstructure:"plugins"`
}

// loadSettings resolves the settings of sec for cmd: flags set on the command line, then the config file,
// then the built-in defaults.
func loadSettings(cmd *cobra.Command, sec section) (settings, error) {
	var s settings
	v, path, err := readConfigFile(cmd)
	if err != nil {
		return s, err
	}
	for _, f := range sec.flags() {
		if err := v.BindPFlag(sec.name+"."+f.Name, cmd.Flags().Lookup(f.Name)); err != nil {
			return s, err
		}
	}
	if err := v.UnmarshalExact(&s); err != nil {
		return s, usagef("invalid config %s: %w", path, err)
	}
	return s, nil
}

// readConfigFile reads the file given by --config, or the default one, which may be missing.
func readConfigFile(cmd *cobra.Command) (v *viper.Viper, path string, err error) {
	v = viper.New()
	v.SetConfigType("yaml")
	path, err = cmd.Flags().GetString("config")
	if err != nil {
		return nil, "", err
	}
	explicit := cmd.Flags().Changed("config")
	if path == "" && !explicit {
		return v, path, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		return v, path, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, path, usagef("config file %s does not exist", path)
	case err != nil:
		return nil, path, err
	}
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return nil, path, usagef("invalid config %s: %w", path, err)
	}
	if err := checkKeys(v, path); err != nil {
		return nil, path, err
	}
	return v, path, nil
}

// replacedTeamKeys are the keys the single preferred team used to have; checkKeys points them out to the
// preferred-teams map.
var replacedTeamKeys = []string{"serve.preferred-team", "serve.score.preferred-team"}

// checkKeys rejects keys of v that no section defines, which catches typos that would otherwise be ignored.
// Everything below the plugins key belongs to the plugins, and the keys below serve.preferred-teams are team
// names; neither is checked.
func checkKeys(v *viper.Viper, path string) error {
	known := make(map[string]bool)
	for _, sec := range sections {
		known[sec.name] = true
		for _, f := range sec.flags() {
			known[sec.name+"."+f.Name] = true
		}
	}
	known[serveSection.name+"."+scoreKey] = true
	for _, t := range scoreTerms {
		known[serveSection.name+"."+scoreKey+"."+t.key] = true
	}
	teamsPrefix := serveSection.name + "." + preferredTeamsKey + "."
	var errs []error
	keys := v.AllKeys()
	slices.Sort(keys)
	for _, key := range keys {
		if known[key] || key == pluginsKey || strings.HasPrefix(key, pluginsKey+".") || strings.HasPrefix(key, teamsPrefix) {
			continue
		}
		if slices.Contains(replacedTeamKeys, key) {
			errs = append(errs, fmt.Errorf("invalid config %s: unknown key %q: replaced by %s.%s (org/slug: points)",
				path, key, serveSection.name, preferredTeamsKey))
			continue
		}
		errs = append(errs, fmt.Errorf("invalid config %s: unknown key %q", path, key))
	}
	if len(errs) > 0 {
		return usageError{errors.Join(errs...)}
	}
	return nil
}

// defaultConfigPath returns $XDG_CONFIG_HOME/dev-inbox/config.yaml, or ~/.config/dev-inbox/config.yaml when unset.
// It returns "" if the home directory is unknown, which leaves no default file to read.
func defaultConfigPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "dev-inbox", "config.yaml")
}

// expandHome replaces a leading ~/ with the home directory.
func expandHome(path string) (string, error) {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~ in %q: %w", path, err)
	}
	return filepath.Join(home, rest), nil
}

// writeConfigTemplate writes every setting commented out at its built-in default. Removing the leading "# " of
// each line yields a valid config file.
func writeConfigTemplate(w io.Writer) error {
	var b strings.Builder
	for i, sec := range sections {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "# %s:\n", sec.name)
		for j, f := range sec.flags() {
			if j > 0 {
				b.WriteString("\n")
			}
			value, err := yamlDefault(f)
			if err != nil {
				return err
			}
			_, usage := pflag.UnquoteUsage(f)
			fmt.Fprintf(&b, "#   # %s\n#   %s: %s\n", usage, f.Name, value)
		}
		if sec.name == serveSection.name {
			writeScoreTemplate(&b)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// yamlDefault renders the default of f as a single-line YAML value; an empty map becomes {}.
func yamlDefault(f *pflag.Flag) (string, error) {
	node := yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: f.DefValue}
	switch v := f.Value.(type) {
	case pflag.SliceValue:
		node = yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, item := range v.GetSlice() {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: item})
		}
	default:
		switch f.Value.Type() {
		case "int":
			node.Tag = "!!int"
		case "stringToInt":
			node = yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
			for _, pair := range strings.Split(strings.Trim(f.DefValue, "[]"), ",") {
				if key, points, ok := strings.Cut(pair, "="); ok {
					node.Content = append(node.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
						&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: points})
				}
			}
		}
	}
	out, err := yaml.Marshal(&node)
	if err != nil {
		return "", fmt.Errorf("render default of %s: %w", f.Name, err)
	}
	return strings.TrimSpace(string(out)), nil
}
