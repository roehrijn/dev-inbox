// Command dev-inbox runs the PR inbox TUI or, with the serve subcommand, the daemon behind it.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/daemon"
	"github.com/roehrijn/dev-inbox/internal/tui"
)

// preferredTeamsKey is the flag and the serve key of the team score boosts.
const preferredTeamsKey = "preferred-teams"

// usageError reports invalid arguments, flags or configuration. It makes the process exit with status 2.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usagef(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

// runners are the long-running parts the commands hand over to.
type runners struct {
	tui   func(ctx context.Context, natsURL string) error
	serve func(ctx context.Context, cfg daemon.Config) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, runners{tui: tui.Run, serve: daemon.Run}, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	var usage usageError
	switch {
	case err == nil:
	case errors.As(err, &usage):
		os.Exit(2)
	default:
		os.Exit(1)
	}
}

// run executes the command line and reports a failure on stderr before returning it.
func run(ctx context.Context, r runners, args []string, stdout, stderr io.Writer) error {
	root := newRootCmd(r)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// Cobra reads os.Args when the arguments are nil.
	root.SetArgs(append([]string{}, args...))
	cmd, err := root.ExecuteContextC(ctx)
	var usage usageError
	switch {
	case err == nil:
	case errors.As(err, &usage):
		_, _ = fmt.Fprintf(stderr, "dev-inbox: %v\nRun '%s --help' for usage.\n", err, cmd.CommandPath())
	default:
		_, _ = fmt.Fprintln(stderr, "dev-inbox:", err)
	}
	return err
}

func newRootCmd(r runners) *cobra.Command {
	root := &cobra.Command{
		Use:   "dev-inbox",
		Short: "Terminal mimic of the GitHub PR inbox",
		Long: "dev-inbox runs the PR inbox TUI, a client of the daemon started with dev-inbox serve.\n\n" +
			"Settings come from flags, then the config file, then built-in defaults.",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := loadSettings(cmd, tuiSection)
			if err != nil {
				return err
			}
			return r.tui(cmd.Context(), s.TUI.NATSURL)
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	tuiSection.define(root.Flags())
	root.PersistentFlags().String("config", defaultConfigPath(),
		"Path of the YAML config file; unlike the default, a file given here must exist")
	root.AddCommand(newServeCmd(r), newConfigCmd())
	return root
}

func newServeCmd(r runners) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon",
		Long:  "Run the daemon that polls GitHub and serves the embedded NATS server the TUI connects to.",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := serveConfig(cmd)
			if err != nil {
				return err
			}
			return r.serve(cmd.Context(), cfg)
		},
	}
	serveSection.define(cmd.Flags())
	return cmd
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage the config file",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "create",
		Short: "Print a config file template to stdout",
		Long: "Print a config file template to stdout. Every setting is commented out at its built-in default, " +
			"for example:\n\n  dev-inbox config create > ~/.config/dev-inbox/config.yaml",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return writeConfigTemplate(cmd.OutOrStdout()) },
	})
	return cmd
}

func noArgs(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return nil
	case cmd.HasSubCommands():
		return usagef("unknown command %q", args[0])
	default:
		return usagef("unexpected argument %q", args[0])
	}
}

func defineTUIFlags(fs *pflag.FlagSet) {
	fs.SortFlags = false
	fs.String("nats-url", api.DefaultNATSURL, "NATS URL of the daemon")
}

func defineServeFlags(fs *pflag.FlagSet) {
	fs.SortFlags = false
	// On error the daemon resolves the default again and reports it.
	stateDir, _ := daemon.DefaultStateDir()
	fs.String("state-dir", stateDir, "Directory of the database and the JetStream storage")
	fs.String("nats-host", daemon.DefaultNATSHost, "Listen address of the embedded NATS server")
	fs.Int("nats-port", daemon.DefaultNATSPort, "Listen port of the embedded NATS server")
	fs.Duration("poll-interval", daemon.DefaultPollInterval, "Minimum interval between GitHub notification polls")
	fs.Duration("reconcile-interval", daemon.DefaultReconcileInterval,
		"Interval between status reconciliations of open PRs")
	fs.String("log-level", "info", "Log level: debug, info, warn or error")
	fs.StringToInt(preferredTeamsKey, nil, "Rank PRs of members of these GitHub teams by `org/slug=points`; "+
		"repeatable, replaces the config file's teams; negative points rank down")
	fs.StringSlice("greylist", slices.Clone(daemon.DefaultGreylist),
		"Rank PRs of this author `login` down, with or without a [bot] suffix; "+
			"repeatable, the first use replaces the defaults, empty disables")
}

// serveConfig merges flags, config file and defaults into the daemon configuration.
func serveConfig(cmd *cobra.Command) (daemon.Config, error) {
	s, err := loadSettings(cmd, serveSection)
	if err != nil {
		return daemon.Config{}, err
	}
	sv := s.Serve
	cfg := daemon.Config{
		NATSHost: sv.NATSHost,
		NATSPort: sv.NATSPort,
		Greylist: sv.Greylist,
	}
	if cfg.StateDir, err = expandHome(sv.StateDir); err != nil {
		return daemon.Config{}, err
	}
	if cfg.PollInterval, err = parseDuration("poll-interval", sv.PollInterval); err != nil {
		return daemon.Config{}, err
	}
	if cfg.ReconcileInterval, err = parseDuration("reconcile-interval", sv.ReconcileInterval); err != nil {
		return daemon.Config{}, err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(sv.LogLevel)); err != nil {
		return daemon.Config{}, usagef("invalid log-level %q: want debug, info, warn or error", sv.LogLevel)
	}
	if cfg.PreferredTeams, err = preferredTeams(sv.PreferredTeams); err != nil {
		return daemon.Config{}, err
	}
	for _, login := range sv.Greylist {
		if strings.TrimSpace(login) == "" {
			return daemon.Config{}, usagef("invalid greylist: login must not be empty")
		}
	}
	if cfg.Weights, err = scoreWeights(sv.Score); err != nil {
		return daemon.Config{}, err
	}
	cfg.Logger = slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))
	return cfg, nil
}

// preferredTeams validates the serve.preferred-teams map: team names of the form org/slug, integer points.
// It returns nil for an empty map.
func preferredTeams(raw map[string]any) (map[string]int, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	teams := make(map[string]int, len(raw))
	for _, team := range slices.Sorted(maps.Keys(raw)) {
		if err := validateTeam(team); err != nil {
			return nil, usagef("invalid %s: %w", preferredTeamsKey, err)
		}
		points, ok := raw[team].(int)
		if !ok {
			return nil, usagef("invalid %s %s: points %v, want an integer", preferredTeamsKey, team, raw[team])
		}
		teams[team] = points
	}
	return teams, nil
}

func parseDuration(name, v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, usagef("invalid %s %q: %w", name, v, err)
	}
	return d, nil
}

// validateTeam checks that team has the form org/slug.
func validateTeam(team string) error {
	org, slug, ok := strings.Cut(team, "/")
	if !ok || org == "" || slug == "" || strings.ContainsAny(team, " \t\n") || strings.Contains(slug, "/") {
		return fmt.Errorf("%q is not of the form org/slug", team)
	}
	return nil
}
