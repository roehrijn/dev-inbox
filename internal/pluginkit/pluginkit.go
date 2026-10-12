// Package pluginkit is the NATS plumbing shared by dev-inbox plugins, which talk to the daemon only
// through its public API.
package pluginkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const RequestTimeout = 10 * time.Second

// ConfigPath returns $XDG_CONFIG_HOME/dev-inbox/<name>.yaml, the config file of the plugin name next to the
// config file of dev-inbox. XDG_CONFIG_HOME counts only when absolute, else ~/.config is used. It returns ""
// if the home directory is unknown, which leaves no default file to read.
func ConfigPath(name string) string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "dev-inbox", name+".yaml")
}

// Client calls the daemon's API. Annotations written through it carry its source.
type Client struct {
	nc     *nats.Conn
	url    string
	source string
	log    *slog.Logger
}

// Connect connects to url and reconnects forever, also when the daemon is not up yet. source names the
// connection and becomes the Source of the annotations written through the client.
func Connect(url, source string, log *slog.Logger) (*Client, error) {
	nc, err := nats.Connect(url,
		nats.Name(source),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", url, err)
	}
	return &Client{nc: nc, url: url, source: source, log: log}, nil
}

func (c *Client) Conn() *nats.Conn { return c.nc }

func (c *Client) Close() { _ = c.nc.Drain() }

// AwaitDaemon lists the inbox, retrying until the daemon answers or ctx ends.
func (c *Client) AwaitDaemon(ctx context.Context) (api.ListResponse, bool) {
	delay := time.Second
	for {
		resp, err := c.List(ctx)
		if err == nil {
			return resp, true
		}
		c.log.Warn("list inbox, is `dev-inbox serve` running?", "err", err, "retry_in_ms", delay.Milliseconds())
		select {
		case <-ctx.Done():
			return resp, false
		case <-time.After(delay):
		}
		delay = min(2*delay, 30*time.Second)
	}
}

func (c *Client) List(ctx context.Context) (api.ListResponse, error) {
	var resp api.ListResponse
	if err := c.Request(ctx, api.SubjectList, api.ListRequest{}, &resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func (c *Client) Get(ctx context.Context, key model.PRKey) (api.InboxItem, error) {
	var resp api.GetResponse
	if err := c.Request(ctx, api.SubjectGet, api.GetRequest{PR: key}, &resp); err != nil {
		return api.InboxItem{}, err
	}
	if resp.Error != "" {
		return api.InboxItem{}, fmt.Errorf("get %s: %s", key, resp.Error)
	}
	if resp.Item == nil {
		return api.InboxItem{}, fmt.Errorf("get %s: no item", key)
	}
	return *resp.Item, nil
}

func (c *Client) Annotate(ctx context.Context, a model.Annotation) error {
	a.Source = c.source
	var resp api.AnnotateResponse
	if err := c.Request(ctx, api.SubjectAnnotate, api.AnnotateRequest{Annotation: a}, &resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("annotate %s %s: %s", a.PR, a.Key, resp.Error)
	}
	return nil
}

func (c *Client) Request(ctx context.Context, subject string, req, resp any) error {
	// While reconnecting, NATS buffers requests until they time out; fail fast instead.
	if !c.nc.IsConnected() {
		return fmt.Errorf("request %s: not connected to %s", subject, c.url)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	msg, err := c.nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		return fmt.Errorf("request %s: %w", subject, err)
	}
	if err := json.Unmarshal(msg.Data, resp); err != nil {
		return fmt.Errorf("decode %s response: %w", subject, err)
	}
	return nil
}

// FilterSubjects returns the subjects of the given event types (model.Event* constants), see
// api.EventSubject.
func FilterSubjects(types []string) []string {
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = "devinbox.events.pr.*.*.*." + strings.TrimPrefix(t, "pr.")
	}
	return out
}

// EventKey returns the PR of a CloudEvent carrying model.EventData.
func EventKey(data []byte) (model.PRKey, error) {
	var ce api.CloudEvent
	if err := json.Unmarshal(data, &ce); err != nil {
		return model.PRKey{}, err
	}
	var ed struct {
		PR struct {
			Key model.PRKey `json:"key"`
		} `json:"pr"`
	}
	if err := json.Unmarshal(ce.Data, &ed); err != nil {
		return model.PRKey{}, err
	}
	k := ed.PR.Key
	if k.Owner == "" || k.Repo == "" || k.Number == 0 {
		return model.PRKey{}, fmt.Errorf("event %s has no PR key", ce.ID)
	}
	return k, nil
}

// Backoff doubles base per previous attempt, capped at maxDelay.
func Backoff(base, maxDelay time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt && d < maxDelay; i++ {
		d *= 2
	}
	return min(d, maxDelay)
}

// Settle acks msg after a successful evaluation, and otherwise redelivers it after delay(deliveries)
// until maxDeliver deliveries.
func Settle(log *slog.Logger, msg jetstream.Msg, evalErr error, delay func(attempt int) time.Duration, maxDeliver int) {
	log = log.With("subject", msg.Subject())
	if evalErr == nil {
		if err := msg.Ack(); err != nil {
			log.Warn("ack event", "err", err)
		}
		return
	}
	md, err := msg.Metadata()
	if err != nil {
		log.Error("evaluation failed, redelivering", "err", evalErr, "metadata_err", err)
		_ = msg.Nak()
		return
	}
	if md.NumDelivered >= uint64(maxDeliver) {
		log.Error("evaluation failed, dropping event", "deliveries", md.NumDelivered, "err", evalErr)
		if err := msg.TermWithReason("max deliveries reached"); err != nil {
			log.Warn("terminate event", "err", err)
		}
		return
	}
	d := delay(int(md.NumDelivered))
	log.Warn("evaluation failed, redelivering", "deliveries", md.NumDelivered,
		"retry_in_ms", d.Milliseconds(), "err", evalErr)
	if err := msg.NakWithDelay(d); err != nil {
		log.Warn("nak event", "err", err)
	}
}
