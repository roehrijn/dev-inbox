// Command dev-inbox-slack is the optional dev-inbox plugin that annotates PRs which tag the viewer in
// Slack review channels, and asks the daemon to track such PRs it does not know yet.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/pluginkit"
	"github.com/roehrijn/dev-inbox/internal/slack"
	"github.com/roehrijn/dev-inbox/internal/slackplugin"
)

// usageError reports invalid arguments, flags or configuration. It makes the process exit with status 2.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usagef(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

// deps are the side effects of the command, replaced in tests.
type deps struct {
	run    func(ctx context.Context, cfg slackplugin.Config) error
	getenv func(string) string
	login  func(ctx context.Context, cfg slack.OAuthConfig, open func(url string) error) (slack.Token, error)
	// newClient returns a Slack client using the token stored in cfg.Store.
	newClient func(cfg slack.Config) (slackplugin.Slack, error)
	browse    func(url string) error
}

func defaultDeps() deps {
	return deps{
		run:    slackplugin.Run,
		getenv: os.Getenv,
		login:  slack.Login,
		newClient: func(cfg slack.Config) (slackplugin.Slack, error) {
			c, err := slack.New(cfg)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		browse: openBrowser,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, defaultDeps(), os.Args[1:], os.Stdout, os.Stderr)
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
func run(ctx context.Context, d deps, args []string, stdout, stderr io.Writer) error {
	root := newRootCmd(d)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// Cobra reads os.Args when the arguments are nil.
	root.SetArgs(append([]string{}, args...))
	err := root.ExecuteContext(ctx)
	var usage usageError
	switch {
	case err == nil:
	case errors.As(err, &usage):
		_, _ = fmt.Fprintf(stderr, "dev-inbox-slack: %v\nRun 'dev-inbox-slack --help' for usage.\n", err)
	default:
		_, _ = fmt.Fprintln(stderr, "dev-inbox-slack:", err)
	}
	return err
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usagef("unexpected argument %q", args[0])
	}
	return nil
}

func newRootCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev-inbox-slack",
		Short: "Annotate PRs that tag you in Slack review channels",
		Long: "dev-inbox-slack reads Slack review channels and annotates the PRs linked in messages that tag you, " +
			"directly or through one of your user groups, with slack.mentions, through the NATS API of dev-inbox serve. " +
			"PRs the daemon does not know yet are tracked first.\n\n" +
			"Settings come from flags, then the config file slack.yaml in the directory of the dev-inbox config file, " +
			"then built-in defaults. The OAuth client ID is read from " + slack.ClientIDEnv + " or else oauth-client-id " +
			"in the config file. Sign in once with 'dev-inbox-slack login'.",
		Args:          noArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := pluginConfig(cmd, d)
			if err != nil {
				return err
			}
			return d.run(cmd.Context(), cfg)
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	cmd.PersistentFlags().String("config", pluginkit.ConfigPath("slack"),
		"Path of the YAML config file; unlike the default, a file given here must exist")
	fs := cmd.Flags()
	fs.SortFlags = false
	fs.String("nats-url", api.DefaultNATSURL,
		"NATS URL of the daemon; the config file's nats-url replaces the default")
	fs.String("log-level", "info", "Log level: debug, info, warn or error")
	cmd.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "Sign in to Slack in the browser and store the token",
		Long: "login signs in to Slack through the browser and stores the token in the dev-inbox state directory. " +
			"The token refreshes itself; sign in again only when the plugin asks for it.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return login(cmd, d) },
	})
	return cmd
}

// readConfig reads the config file named by --config and the OAuth client ID.
func readConfig(cmd *cobra.Command, getenv func(string) string) (settings, string, error) {
	fs := cmd.Flags()
	path, _ := fs.GetString("config")
	s, err := readSettings(path, fs.Changed("config"))
	if err != nil {
		return s, path, err
	}
	if id := strings.TrimSpace(getenv(slack.ClientIDEnv)); id != "" {
		s.ClientID = id
	}
	if s.ClientID == "" {
		where := "the config file"
		if path != "" {
			where = path
		}
		return s, path, usagef("no Slack OAuth client ID: set %s or oauth-client-id in %s", slack.ClientIDEnv, where)
	}
	return s, path, nil
}

// pluginConfig merges flags, config file and defaults into the plugin configuration.
func pluginConfig(cmd *cobra.Command, d deps) (slackplugin.Config, error) {
	s, path, err := readConfig(cmd, d.getenv)
	if err != nil {
		return slackplugin.Config{}, err
	}
	fs := cmd.Flags()
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
		return slackplugin.Config{}, usagef("invalid log-level %q: want debug, info, warn or error", levelText)
	}
	if len(s.Channels) == 0 {
		return slackplugin.Config{}, usagef("no channels to read: set channels in %s", path)
	}

	logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))
	client, err := d.newClient(slack.Config{OAuth: slack.OAuthConfig{ClientID: s.ClientID}, Logger: logger})
	if err != nil {
		return slackplugin.Config{}, err
	}
	return slackplugin.Config{
		NATSURL:      natsURL,
		Channels:     s.Channels,
		Groups:       s.Groups,
		Score:        s.Score,
		PollInterval: s.PollInterval,
		Lookback:     s.Lookback,
		Slack:        client,
		Logger:       logger,
	}, nil
}

// login runs the browser sign-in, stores the token and prints the signed-in user.
func login(cmd *cobra.Command, d deps) error {
	s, _, err := readConfig(cmd, d.getenv)
	if err != nil {
		return err
	}
	ctx, stderr := cmd.Context(), cmd.ErrOrStderr()
	oauth := slack.OAuthConfig{ClientID: s.ClientID}
	token, err := d.login(ctx, oauth, func(url string) error {
		_, _ = fmt.Fprintf(stderr, "Sign in to Slack in the browser. If it does not open, visit:\n%s\n", url)
		if err := d.browse(url); err != nil {
			_, _ = fmt.Fprintln(stderr, "Could not open the browser:", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("slack login: %w", err)
	}
	path, err := slack.DefaultTokenPath()
	if err != nil {
		return err
	}
	store := slack.TokenStore{Path: path}
	if err := store.Save(token); err != nil {
		return fmt.Errorf("save slack token: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client, err := d.newClient(slack.Config{OAuth: oauth, Store: store, Logger: logger})
	if err != nil {
		return err
	}
	id, err := client.AuthTest(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "signed in as", id.UserID)
	return err
}

func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
