// Package bus runs the embedded NATS server with JetStream, publishes events as CloudEvents and
// hosts the request/reply API and in-process event handlers.
package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/roehrijn/dev-inbox/internal/api"
	"github.com/roehrijn/dev-inbox/internal/model"
)

const (
	defaultHost   = "127.0.0.1"
	defaultPort   = 14222
	defaultMaxAge = 30 * 24 * time.Hour

	duplicateWindow = 2 * time.Minute
	startTimeout    = 10 * time.Second

	relayBatchSize = 100

	// handlerTimeout stays below handlerAckWait so the server never redelivers an event whose
	// handler is still running.
	handlerTimeout = time.Minute
	handlerAckWait = 2 * time.Minute

	apiQueueGroup = "devinbox-api"
	serveTimeout  = 30 * time.Second
)

type Config struct {
	Host     string        // default 127.0.0.1
	Port     int           // default 14222, -1 picks a random free port
	StoreDir string        // required; JetStream keeps its data in StoreDir/jetstream
	MaxAge   time.Duration // event retention in the stream, default 30 days
	Logger   *slog.Logger  // default slog.Default()
}

type Bus struct {
	ns  *server.Server
	nc  *nats.Conn
	js  jetstream.JetStream
	log *slog.Logger

	kick       chan struct{}
	connClosed chan struct{}
	// ctx parents the contexts of Serve requests and ends on Close.
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	closed    bool           // guarded by mu; no handlers are added once set
	handlers  sync.WaitGroup // consume goroutines started by Handle

	relayPoll       time.Duration
	relayBackoffMin time.Duration
	relayBackoffMax time.Duration

	retryBase  time.Duration
	retryMax   time.Duration
	maxDeliver uint64
}

// Start runs the embedded server, waits until it accepts connections and ensures the events stream exists.
func Start(cfg Config) (*Bus, error) {
	if cfg.StoreDir == "" {
		return nil, errors.New("bus: StoreDir is required")
	}
	if cfg.Host == "" {
		cfg.Host = defaultHost
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = defaultMaxAge
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	ns, err := server.NewServer(&server.Options{
		ServerName:             "dev-inbox",
		Host:                   cfg.Host,
		Port:                   cfg.Port,
		JetStream:              true,
		StoreDir:               cfg.StoreDir,
		NoSigs:                 true,
		DisableJetStreamBanner: true,
	})
	if err != nil {
		return nil, fmt.Errorf("configure nats server: %w", err)
	}
	srvLog := &serverLogger{log: cfg.Logger.With("component", "nats-server"), fatal: make(chan error, 1)}
	ns.SetLoggerV2(srvLog, false, false, false)

	// Start returns after JetStream is enabled and the client listener is bound, reporting failures
	// of either only through Fatalf.
	ns.Start()
	select {
	case err := <-srvLog.fatal:
		ns.Shutdown()
		return nil, fmt.Errorf("start nats server on %s:%d: %w", cfg.Host, cfg.Port, err)
	default:
	}
	if !ns.ReadyForConnections(startTimeout) {
		ns.Shutdown()
		return nil, fmt.Errorf("nats server on %s:%d not ready after %s", cfg.Host, cfg.Port, startTimeout)
	}

	b := &Bus{
		ns:              ns,
		log:             cfg.Logger,
		kick:            make(chan struct{}, 1),
		connClosed:      make(chan struct{}),
		relayPoll:       5 * time.Second,
		relayBackoffMin: time.Second,
		relayBackoffMax: 30 * time.Second,
		retryBase:       time.Second,
		retryMax:        5 * time.Minute,
		maxDeliver:      10,
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())

	b.nc, err = nats.Connect("", nats.InProcessServer(ns), nats.Name("dev-inbox-daemon"),
		nats.ClosedHandler(func(*nats.Conn) { close(b.connClosed) }))
	if err != nil {
		b.cancel()
		ns.Shutdown()
		return nil, fmt.Errorf("connect to nats server: %w", err)
	}
	if err := b.ensureStream(cfg.MaxAge); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

func (b *Bus) ensureStream(maxAge time.Duration) error {
	js, err := jetstream.New(b.nc)
	if err != nil {
		return fmt.Errorf("jetstream context: %w", err)
	}
	b.js = js
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       api.StreamName,
		Subjects:   []string{api.EventsSubjectAll},
		Storage:    jetstream.FileStorage,
		MaxAge:     maxAge,
		Duplicates: duplicateWindow,
	})
	if err != nil {
		return fmt.Errorf("create or update stream %s: %w", api.StreamName, err)
	}
	return nil
}

func (b *Bus) ClientURL() string { return b.ns.ClientURL() }

// Conn returns the bus's own in-process client connection.
func (b *Bus) Conn() *nats.Conn { return b.nc }

// Close drains connections, waits for in-flight handler calls and shuts the server down.
func (b *Bus) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		if err := b.nc.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			b.closeErr = fmt.Errorf("drain nats connection: %w", err)
			b.nc.Close()
		}
		<-b.connClosed
		// Handler calls outliving the drain timeout still finish before the server goes away.
		b.handlers.Wait()
		b.cancel()
		b.ns.Shutdown()
		b.ns.WaitForShutdown()
	})
	return b.closeErr
}

// Publish publishes ev as a CloudEvent on api.EventSubject(ev), deduplicated by event ID, and waits
// for the JetStream ack.
func (b *Bus) Publish(ctx context.Context, ev model.Event) error {
	body, err := json.Marshal(api.NewCloudEvent(ev))
	if err != nil {
		return fmt.Errorf("encode event %s: %w", ev.ID, err)
	}
	msg := nats.NewMsg(api.EventSubject(ev))
	msg.Header.Set(jetstream.MsgIDHeader, ev.ID)
	msg.Header.Set("Content-Type", api.CloudEventContentType)
	msg.Data = body
	if _, err := b.js.PublishMsg(ctx, msg, jetstream.WithExpectStream(api.StreamName)); err != nil {
		return fmt.Errorf("publish event %s: %w", ev.ID, err)
	}
	return nil
}

// Outbox is the source of events for RunRelay; *store.Store implements it.
type Outbox interface {
	PendingEvents(ctx context.Context, limit int) ([]model.Event, error)
	MarkPublished(ctx context.Context, ids []string) error
}

// RunRelay publishes pending outbox events in order until ctx ends. It polls periodically and
// immediately after Kick. A failed publish is retried with backoff before any later event is
// published. It returns nil when ctx ends.
func (b *Bus) RunRelay(ctx context.Context, src Outbox) error {
	backoff := b.relayBackoffMin
	for {
		more, err := b.relayBatch(ctx, src)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			b.log.Warn("relay outbox events", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, b.relayBackoffMax)
			continue
		}
		backoff = b.relayBackoffMin
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-b.kick:
		case <-time.After(b.relayPoll):
		}
	}
}

// relayBatch publishes one batch of pending events and marks the published prefix. more reports a
// full batch, so further events may be pending.
func (b *Bus) relayBatch(ctx context.Context, src Outbox) (more bool, err error) {
	evs, err := src.PendingEvents(ctx, relayBatchSize)
	if err != nil {
		return false, fmt.Errorf("read outbox: %w", err)
	}
	ids := make([]string, 0, len(evs))
	var pubErr error
	for _, ev := range evs {
		if pubErr = b.Publish(ctx, ev); pubErr != nil {
			break
		}
		ids = append(ids, ev.ID)
	}
	if len(ids) > 0 {
		// Record acked events even when ctx just ended; otherwise they are published again after
		// the duplicate window.
		if err := src.MarkPublished(context.WithoutCancel(ctx), ids); err != nil {
			return false, fmt.Errorf("mark events published: %w", err)
		}
	}
	return pubErr == nil && len(evs) == relayBatchSize, pubErr
}

// Kick wakes the relay after new events were committed to the outbox. It never blocks.
func (b *Bus) Kick() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

type HandlerFunc func(ctx context.Context, ev api.CloudEvent) error

// Handle runs fn for every event matching filterSubject through a durable JetStream consumer named
// name, so a handler resumes where it stopped after restarts. Events whose handler fails are
// redelivered with backoff; after 10 failed deliveries the event is dropped and logged. fn's context
// ends after one minute.
//
// Handle returns once the consumer exists, so a newly created consumer receives every event
// published after Handle returns, but none from before. Handling then continues in the background
// until ctx ends or the bus is closed; Close waits for in-flight handler calls.
func (b *Bus) Handle(ctx context.Context, name, filterSubject string, fn HandlerFunc) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return fmt.Errorf("handle %s: bus closed", name)
	}
	b.handlers.Add(1)
	b.mu.Unlock()

	cc, err := b.consume(ctx, name, filterSubject, fn)
	if err != nil {
		b.handlers.Done()
		return err
	}
	go func() {
		defer b.handlers.Done()
		select {
		case <-ctx.Done():
		case <-cc.Closed():
			b.mu.Lock()
			closed := b.closed
			b.mu.Unlock()
			if !closed && ctx.Err() == nil {
				b.log.Error("event handler stopped", "handler", name)
			}
		}
		cc.Stop()
		<-cc.Closed()
	}()
	return nil
}

func (b *Bus) consume(ctx context.Context, name, filterSubject string, fn HandlerFunc) (jetstream.ConsumeContext, error) {
	cons, err := b.js.CreateOrUpdateConsumer(ctx, api.StreamName, jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: filterSubject,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       handlerAckWait,
		MaxDeliver:    -1,
	})
	if err != nil {
		return nil, fmt.Errorf("create consumer %s: %w", name, err)
	}
	log := b.log.With("handler", name)
	// One message at a time: buffered messages would count against AckWait while earlier ones
	// are handled, and would be discarded unacked on stop.
	cc, err := cons.Consume(func(msg jetstream.Msg) { b.handleMsg(ctx, log, msg, fn) },
		jetstream.PullMaxMessages(1),
		jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
			if ctx.Err() == nil {
				log.Warn("consume events", "err", err)
			}
		}))
	if err != nil {
		return nil, fmt.Errorf("consume %s: %w", name, err)
	}
	return cc, nil
}

func (b *Bus) handleMsg(ctx context.Context, log *slog.Logger, msg jetstream.Msg, fn HandlerFunc) {
	log = log.With("subject", msg.Subject())
	var ev api.CloudEvent
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		log.Error("dropping undecodable event", "err", err)
		if err := msg.TermWithReason("undecodable payload"); err != nil {
			log.Warn("terminate event", "err", err)
		}
		return
	}
	log = log.With("event", ev.ID, "type", ev.Type)

	hctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	herr := fn(hctx, ev)
	cancel()
	if herr == nil {
		if err := msg.Ack(); err != nil {
			log.Warn("ack event", "err", err)
		}
		return
	}

	md, err := msg.Metadata()
	if err != nil {
		log.Error("handler failed, redelivering", "err", herr, "metadata_err", err)
		_ = msg.Nak()
		return
	}
	if md.NumDelivered >= b.maxDeliver {
		log.Error("handler failed, dropping event", "deliveries", md.NumDelivered, "err", herr)
		if err := msg.TermWithReason("max deliveries reached"); err != nil {
			log.Warn("terminate event", "err", err)
		}
		return
	}
	delay := b.retryDelay(md.NumDelivered)
	log.Warn("handler failed, redelivering", "deliveries", md.NumDelivered, "retry_in", delay, "err", herr)
	if err := msg.NakWithDelay(delay); err != nil {
		log.Warn("nak event", "err", err)
	}
}

// retryDelay doubles retryBase per previous delivery, capped at retryMax.
func (b *Bus) retryDelay(numDelivered uint64) time.Duration {
	d := b.retryBase
	for i := uint64(1); i < numDelivered && d < b.retryMax; i++ {
		d *= 2
	}
	return min(d, b.retryMax)
}

type errorReply struct {
	Error string `json:"error"`
}

// Serve answers requests on subject with the JSON encoding of fn's result. When fn fails the reply is
// {"error": "..."}. Requests are handled one at a time per subject, each with a 30 second context.
func (b *Bus) Serve(subject string, fn func(ctx context.Context, req []byte) (any, error)) error {
	_, err := b.nc.QueueSubscribe(subject, apiQueueGroup, func(msg *nats.Msg) {
		ctx, cancel := context.WithTimeout(b.ctx, serveTimeout)
		defer cancel()
		res, err := fn(ctx, msg.Data)
		var body []byte
		if err == nil {
			body, err = json.Marshal(res)
		}
		if err != nil {
			body, _ = json.Marshal(errorReply{Error: err.Error()})
		}
		if msg.Reply == "" {
			return
		}
		if err := msg.Respond(body); err != nil {
			b.log.Warn("reply to request", "subject", subject, "err", err)
		}
	})
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", subject, err)
	}
	// Requests sent right after Serve returns must already find the subscription.
	if err := b.nc.Flush(); err != nil {
		return fmt.Errorf("subscribe %s: %w", subject, err)
	}
	return nil
}

// serverLogger forwards warnings and errors of the embedded server to slog and reports fatal
// errors, which the server only logs, on fatal.
type serverLogger struct {
	log   *slog.Logger
	fatal chan error
}

func (l *serverLogger) Noticef(string, ...any) {}
func (l *serverLogger) Debugf(string, ...any)  {}
func (l *serverLogger) Tracef(string, ...any)  {}

func (l *serverLogger) Warnf(format string, v ...any) { l.log.Warn(fmt.Sprintf(format, v...)) }

func (l *serverLogger) Errorf(format string, v ...any) { l.log.Error(fmt.Sprintf(format, v...)) }

func (l *serverLogger) Fatalf(format string, v ...any) {
	err := fmt.Errorf(format, v...)
	l.log.Error(err.Error())
	select {
	case l.fatal <- err:
	default:
	}
}
