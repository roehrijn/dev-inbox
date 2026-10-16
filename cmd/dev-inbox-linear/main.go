// Command dev-inbox-linear is the optional dev-inbox plugin that links PRs to Linear issues and
// annotates the PRs whose issues, projects or milestones are due soon.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/linear"
	"github.com/roehrijn/dev-inbox/internal/linearplugin"
	"github.com/roehrijn/dev-inbox/internal/pluginkit"
)

// usageError reports invalid arguments, flags or configuration. It makes the process exit with status 2.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usagef(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

type runFunc func(ctx context.Context, cfg linearplugin.Config) error

// version is set by the release build.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, linearplugin.Run, os.Getenv, os.Args[1:], os.Stdout, os.Stderr)
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

// run executes the command line and reports a failure on stderr before returning it. Logs go to
// stderr as JSON lines.
func run(ctx context.Context, r runFunc, getenv func(string) string, args []string, stdout, stderr io.Writer) error {
	root := newRootCmd(r, getenv)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// Cobra reads os.Args when the arguments are nil.
	root.SetArgs(append([]string{}, args...))
	err := root.ExecuteContext(ctx)
	var usage usageError
	switch {
	case err == nil:
	case errors.As(err, &usage):
		_, _ = fmt.Fprintf(stderr, "dev-inbox-linear: %v\nRun 'dev-inbox-linear --help' for usage.\n", err)
	default:
		_, _ = fmt.Fprintln(stderr, "dev-inbox-linear:", err)
	}
	return err
}

func newRootCmd(r runFunc, getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "dev-inbox-linear",
		Version: version,
		Short:   "Annotate PRs whose Linear issues are due soon",
		Long: "dev-inbox-linear links PRs to Linear issues and annotates the PRs whose issues, projects or milestones " +
			"are due soon with linear.urgency, through the NATS API of dev-inbox serve.\n\n" +
			"Settings come from flags, then the config file linear.yaml in the directory of the dev-inbox config file, " +
			"then built-in defaults. " +
			"The API key is read from " + linear.APIKeyEnv + " only.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usagef("unexpected argument %q", args[0])
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := pluginConfig(cmd, getenv)
			if err != nil {
				return err
			}
			return r(cmd.Context(), cfg)
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	fs := cmd.Flags()
	fs.SortFlags = false
	fs.String("config", pluginkit.ConfigPath("linear"),
		"Path of the YAML config file; unlike the default, a file given here must exist")
	fs.String("nats-url", api.DefaultNATSURL,
		"NATS URL of the daemon; the config file's nats-url replaces the default")
	fs.String("log-level", "info", "Log level: debug, info, warn or error")
	return cmd
}

// pluginConfig merges flags, config file and defaults into the plugin configuration.
func pluginConfig(cmd *cobra.Command, getenv func(string) string) (linearplugin.Config, error) {
	fs := cmd.Flags()
	path, _ := fs.GetString("config")
	s, err := readSettings(path, fs.Changed("config"))
	if err != nil {
		return linearplugin.Config{}, err
	}
	natsURL, _ := fs.GetString("nats-url")
	if !fs.Changed("nats-url") && s.NATSURL != "" {
		natsURL = s.NATSURL
	}
	levelText, _ := fs.GetString("log-level")
	if !fs.Changed("log-level") && s.LogLevel != "" {
		levelText = s.LogLevel
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(levelText)); err != nil {
		return linearplugin.Config{}, usagef("invalid log-level %q: want debug, info, warn or error", levelText)
	}
	key := strings.TrimSpace(getenv(linear.APIKeyEnv))
	if key == "" {
		return linearplugin.Config{}, usagef("%s is not set", linear.APIKeyEnv)
	}

	logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))
	client, err := linear.New(linear.Config{APIKey: key, Logger: logger})
	if err != nil {
		return linearplugin.Config{}, err
	}
	return linearplugin.Config{
		NATSURL:       natsURL,
		HorizonDays:   s.HorizonDays,
		SweepInterval: s.SweepInterval,
		Linear:        client,
		Logger:        logger,
	}, nil
}
