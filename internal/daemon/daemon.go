// Package daemon wires GitHub ingestion, the store and the bus into the dev-inbox daemon.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/roehrijn/dev-inbox/internal/bus"
	"github.com/roehrijn/dev-inbox/internal/derive"
	"github.com/roehrijn/dev-inbox/internal/github"
	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/store"
)

const (
	DefaultNATSHost          = "127.0.0.1"
	DefaultNATSPort          = 14222
	DefaultPollInterval      = time.Minute
	DefaultReconcileInterval = 5 * time.Minute

	viewerRefreshInterval = 6 * time.Hour
	pruneInterval         = 24 * time.Hour
	eventRetention        = 30 * 24 * time.Hour
	// retryInterval spaces out retries of startup steps that need GitHub, e.g. while offline.
	retryInterval = time.Minute
)

// Meta keys of daemon bookkeeping in the store.
const (
	metaBackfillDone       = "backfill_done"
	metaViewer             = "viewer"
	metaPreferredTeams     = "preferred_teams"
	metaNotificationsSince = "notifications_since"
	metaLastModified       = "notifications_last_modified"
)

// DefaultGreylist holds the logins whose PRs are ranked down unless Config.Greylist says otherwise.
var DefaultGreylist = []string{"dependabot", "renovate"}

type Config struct {
	StateDir          string        // default DefaultStateDir()
	NATSHost          string        // default 127.0.0.1
	NATSPort          int           // default 14222; -1 picks a random port
	PollInterval      time.Duration // minimum notifications poll interval, default 1m
	ReconcileInterval time.Duration // default 5m
	Logger            *slog.Logger  // default slog.Default()
	// PreferredTeams maps "org/slug" to the points PRs of the team's members get; see derive.TeamBoost.
	PreferredTeams map[string]int
	Greylist       []string       // used as given; callers wanting the defaults pass DefaultGreylist
	RoleTeams      []string       // "org/slug"; see derive.Priority.Roles
	Weights        derive.Weights // used as given; callers wanting the defaults pass derive.DefaultWeights
}

// GitHub is the part of *github.Client the daemon uses.
type GitHub interface {
	Viewer(ctx context.Context) (model.Viewer, error)
	Notifications(ctx context.Context, since time.Time, lastModified string) (github.NotificationsResult, error)
	SearchOpenPRs(ctx context.Context, query string) ([]model.PRKey, error)
	FetchPR(ctx context.Context, key model.PRKey) (model.PR, error)
	FetchStatuses(ctx context.Context, keys []model.PRKey) (map[model.PRKey]model.PRStatus, error)
	TeamMembers(ctx context.Context, team string) ([]string, error)
}

// DefaultStateDir returns $XDG_STATE_HOME/dev-inbox, or ~/.local/state/dev-inbox when unset.
func DefaultStateDir() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "dev-inbox"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "dev-inbox"), nil
}

// Run runs the daemon until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	gh, err := github.NewClient()
	if err != nil {
		return err
	}
	gh.SetLogger(cfg.Logger)
	d, err := start(cfg, gh)
	if err != nil {
		return err
	}
	return d.run(ctx)
}

type daemon struct {
	cfg   Config
	log   *slog.Logger
	gh    GitHub
	store *store.Store
	bus   *bus.Bus
	gate  *gate
	queue *queue

	teams []string // configured preferred teams, sorted

	mu     sync.RWMutex
	viewer model.Viewer
	// teamMembers holds the known members by configured team; a team without an entry has none yet.
	teamMembers map[string][]string
}

// start opens the store, starts the bus and serves the API. GitHub is not contacted yet.
func start(cfg Config, gh GitHub) (*daemon, error) {
	if cfg.StateDir == "" {
		dir, err := DefaultStateDir()
		if err != nil {
			return nil, err
		}
		cfg.StateDir = dir
	}
	if cfg.NATSHost == "" {
		cfg.NATSHost = DefaultNATSHost
	}
	if cfg.NATSPort == 0 {
		cfg.NATSPort = DefaultNATSPort
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = DefaultReconcileInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	st, err := store.Open(filepath.Join(cfg.StateDir, "inbox.db"))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	b, err := bus.Start(bus.Config{
		Host:     cfg.NATSHost,
		Port:     cfg.NATSPort,
		StoreDir: cfg.StateDir, // JetStream adds the jetstream/ subdirectory
		Logger:   cfg.Logger,
	})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("start bus: %w", err)
	}
	d := &daemon{
		cfg:   cfg,
		log:   cfg.Logger,
		gh:    gh,
		store: st,
		bus:   b,
		gate:  &gate{log: cfg.Logger},
		queue: newQueue(),
		teams: slices.Sorted(maps.Keys(cfg.PreferredTeams)),

		teamMembers: map[string][]string{},
	}
	if err := d.loadCachedViewer(context.Background()); err != nil {
		d.log.Warn("ignoring cached viewer", "err", err)
	}
	if err := d.loadCachedTeams(context.Background()); err != nil {
		d.log.Warn("ignoring cached preferred teams", "teams", d.teams, "err", err)
	}
	if err := d.serveAPI(); err != nil {
		d.close()
		return nil, err
	}
	return d, nil
}

// run starts the loops and blocks until ctx ends. The worker finishes its current item, then the
// bus and the store are closed.
func (d *daemon) run(ctx context.Context) error {
	defer d.close()

	relayCtx, stopRelay := context.WithCancel(context.WithoutCancel(ctx))
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		if err := d.bus.RunRelay(relayCtx, d.store); err != nil && relayCtx.Err() == nil {
			d.log.Error("event relay stopped", "err", err)
		}
	}()
	defer func() {
		stopRelay()
		<-relayDone
	}()

	d.log.Info("daemon started", "nats", d.bus.ClientURL(), "stateDir", d.cfg.StateDir)
	if err := d.ensureViewer(ctx); err != nil {
		return nil
	}

	var wg sync.WaitGroup
	if len(d.teams) > 0 {
		// Fetched before the worker starts so that the first ingested events carry the final score.
		wait := d.refreshTeams(ctx)
		wg.Go(func() { d.teamsLoop(ctx, wait) })
	}
	for _, loop := range []func(context.Context){
		d.worker,
		d.backfill,
		d.notificationsLoop,
		d.reconcileLoop,
		d.pruneLoop,
		d.viewerLoop,
	} {
		wg.Go(func() { loop(ctx) })
	}
	wg.Wait()
	d.log.Info("daemon stopped")
	return nil
}

func (d *daemon) close() {
	if err := d.bus.Close(); err != nil {
		d.log.Error("close bus", "err", err)
	}
	if err := d.store.Close(); err != nil {
		d.log.Error("close store", "err", err)
	}
}

func (d *daemon) currentViewer() model.Viewer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.viewer
}

func (d *daemon) setViewer(v model.Viewer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.viewer = v
}

// inputs returns the derive inputs for a PR with the given annotations.
func (d *daemon) inputs(annotations []model.Annotation) derive.Inputs {
	d.mu.RLock()
	defer d.mu.RUnlock()
	teams := make([]derive.TeamBoost, 0, len(d.teams))
	for _, team := range d.teams {
		teams = append(teams, derive.TeamBoost{Team: team, Points: d.cfg.PreferredTeams[team], Members: d.teamMembers[team]})
	}
	return derive.Inputs{
		Viewer: d.viewer,
		Priority: derive.Priority{
			Teams:    teams,
			Greylist: d.cfg.Greylist,
			Roles:    d.cfg.RoleTeams,
			Weights:  d.cfg.Weights,
		},
		Annotations: annotations,
		Now:         time.Now(),
	}
}

func (d *daemon) setTeamMembers(team string, members []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.teamMembers[team] = members
}

func (d *daemon) loadCachedViewer(ctx context.Context) error {
	raw, ok, err := d.store.Meta(ctx, metaViewer)
	if err != nil || !ok {
		return err
	}
	var v model.Viewer
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return fmt.Errorf("decode cached viewer: %w", err)
	}
	d.setViewer(v)
	return nil
}

func (d *daemon) refreshViewer(ctx context.Context) error {
	v, err := call(ctx, d.gate, func() (model.Viewer, error) { return d.gh.Viewer(ctx) })
	if err != nil {
		return err
	}
	if v.Login == "" {
		return errors.New("fetch viewer: empty login")
	}
	d.setViewer(v)
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return d.store.SetMeta(ctx, metaViewer, string(raw))
}

// ensureViewer refreshes the viewer. Without GitHub a cached viewer suffices; without both it retries
// until it succeeds, since classification and diffs depend on the viewer.
func (d *daemon) ensureViewer(ctx context.Context) error {
	for {
		err := d.refreshViewer(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.currentViewer().Login != "" {
			d.log.Warn("using cached viewer", "err", err)
			return nil
		}
		d.log.Error("viewer unknown, retrying", "err", err, "in", retryInterval)
		if !sleep(ctx, retryInterval) {
			return ctx.Err()
		}
	}
}

func (d *daemon) viewerLoop(ctx context.Context) {
	for sleep(ctx, viewerRefreshInterval) {
		if err := d.refreshViewer(ctx); err != nil && ctx.Err() == nil {
			d.log.Warn("viewer refresh failed", "err", err)
		}
	}
}

// loadCachedTeams restores the members of the configured teams, so that a start without GitHub still
// ranks PRs. Cached teams that are no longer configured are ignored.
func (d *daemon) loadCachedTeams(ctx context.Context) error {
	if len(d.teams) == 0 {
		return nil
	}
	raw, ok, err := d.store.Meta(ctx, metaPreferredTeams)
	if err != nil || !ok {
		return err
	}
	var cached map[string][]string
	if err := json.Unmarshal([]byte(raw), &cached); err != nil {
		return fmt.Errorf("decode cached preferred teams: %w", err)
	}
	for _, team := range d.teams {
		for name, members := range cached {
			if strings.EqualFold(name, team) {
				d.setTeamMembers(team, members)
			}
		}
	}
	return nil
}

// refreshTeams fetches the members of every preferred team and caches them. It logs failures and
// returns how long to wait before the next refresh. A team whose fetch failed keeps its previous
// members, and the refresh is retried soon.
func (d *daemon) refreshTeams(ctx context.Context) time.Duration {
	wait := viewerRefreshInterval
	for _, team := range d.teams {
		members, err := call(ctx, d.gate, func() ([]string, error) { return d.gh.TeamMembers(ctx, team) })
		switch {
		case ctx.Err() != nil:
			return wait
		case errors.Is(err, github.ErrNotFound):
			d.log.Warn("preferred team not found or not accessible, ranking without it", "team", team, "err", err)
			members = nil
		case err != nil:
			d.log.Warn("preferred team refresh failed", "team", team, "err", err, "in", retryInterval)
			wait = retryInterval
			continue
		}
		d.setTeamMembers(team, members)
	}
	d.cacheTeams(ctx)
	return wait
}

func (d *daemon) cacheTeams(ctx context.Context) {
	d.mu.RLock()
	raw, err := json.Marshal(d.teamMembers)
	d.mu.RUnlock()
	if err == nil {
		err = d.store.SetMeta(ctx, metaPreferredTeams, string(raw))
	}
	if err != nil && ctx.Err() == nil {
		d.log.Warn("cache preferred teams", "teams", d.teams, "err", err)
	}
}

// teamsLoop refreshes the preferred teams after wait and then at the pace refreshTeams asks for.
func (d *daemon) teamsLoop(ctx context.Context, wait time.Duration) {
	for sleep(ctx, wait) {
		wait = d.refreshTeams(ctx)
	}
}

func (d *daemon) pruneLoop(ctx context.Context) {
	for {
		n, err := d.store.PruneEvents(ctx, time.Now().Add(-eventRetention))
		if err != nil && ctx.Err() == nil {
			d.log.Error("prune events", "err", err)
		} else if n > 0 {
			d.log.Info("pruned published events", "count", n)
		}
		if !sleep(ctx, pruneInterval) {
			return
		}
	}
}

// sleep waits for d and reports false when ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
