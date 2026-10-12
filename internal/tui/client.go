package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const requestTimeout = 5 * time.Second

// backend is the daemon API as seen by the model; client implements it over NATS.
type backend interface {
	List(ctx context.Context) (api.ListResponse, error)
	Get(ctx context.Context, key model.PRKey) (api.GetResponse, error)
}

type client struct {
	nc  *nats.Conn
	url string
}

// dial connects to natsURL and keeps reconnecting forever. onState reports every
// connection state change; it is called from NATS goroutines.
func dial(natsURL string, onState func(connected bool)) (*client, error) {
	nc, err := nats.Connect(natsURL,
		nats.Name("dev-inbox-tui"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.ConnectHandler(func(*nats.Conn) { onState(true) }),
		nats.ReconnectHandler(func(*nats.Conn) { onState(true) }),
		nats.DisconnectErrHandler(func(*nats.Conn, error) { onState(false) }),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", natsURL, err)
	}
	return &client{nc: nc, url: natsURL}, nil
}

func (c *client) Close() { c.nc.Close() }

func (c *client) List(ctx context.Context) (api.ListResponse, error) {
	var resp api.ListResponse
	if err := c.request(ctx, api.SubjectList, api.ListRequest{}, &resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func (c *client) Get(ctx context.Context, key model.PRKey) (api.GetResponse, error) {
	var resp api.GetResponse
	if err := c.request(ctx, api.SubjectGet, api.GetRequest{PR: key}, &resp); err != nil {
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	if resp.Item == nil {
		return resp, fmt.Errorf("%s not found", key)
	}
	return resp, nil
}

func (c *client) request(ctx context.Context, subject string, req, resp any) error {
	// While reconnecting, NATS buffers requests until they time out; fail fast instead.
	if !c.nc.IsConnected() {
		return fmt.Errorf("daemon not reachable at %s", c.url)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	msg, err := c.nc.RequestWithContext(ctx, subject, data)
	if errors.Is(err, nats.ErrNoResponders) {
		return fmt.Errorf("no daemon answers %s, is `dev-inbox serve` running?", subject)
	}
	if err != nil {
		return fmt.Errorf("request %s: %w", subject, err)
	}
	if err := json.Unmarshal(msg.Data, resp); err != nil {
		return fmt.Errorf("decode %s response: %w", subject, err)
	}
	return nil
}

// subscribeEvents calls fn for every published event with the key of the affected PR,
// or nil when the event cannot be decoded.
func (c *client) subscribeEvents(fn func(key *model.PRKey)) (*nats.Subscription, error) {
	return c.nc.Subscribe(api.EventsSubjectAll, func(msg *nats.Msg) {
		fn(eventKey(msg.Data))
	})
}

func eventKey(data []byte) *model.PRKey {
	var ce struct {
		Data struct {
			PR struct {
				Key model.PRKey `json:"key"`
			} `json:"pr"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &ce); err != nil || ce.Data.PR.Key.Number == 0 {
		return nil
	}
	return &ce.Data.PR.Key
}
