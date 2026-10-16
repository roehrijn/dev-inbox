// Package jev is a minimal client of TypeSafe AI's System One API, whose model is Jev.
//
// There is no Go SDK. The wire format follows the official SDKs: @typesafe-ai/sdk 0.6.0 on npm
// (dist/index.mjs: TypeSafeClient.systemOne, fetchWithRetries, APIError.fromResponse) and
// typesafe-sdk 0.7.4 on PyPI (typesafe_sdk/_schemas/models.py, generated from
// https://api.typesafe.ai/openapi.json, and _core/errors.py), checked against
// https://docs.typesafe.ai/api.md. Retries mirror the SDKs' default policy: two retries on 408, 429,
// 5xx and transport errors, exponential backoff from 500ms capped at 5s with 25% jitter, and a
// server's Retry-After honored up to a minute.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/roehrijn/dev-inbox/internal/httpx"
)

const (
	DefaultBaseURL    = "https://api.typesafe.ai"
	DefaultModel      = "jev-latest"
	DefaultTimeout    = 10 * time.Second
	DefaultMaxRetries = 2
	// APIKeyEnv is the environment variable the official SDKs read the API key from.
	APIKeyEnv = "TYPESAFE_API_KEY"

	systemOnePath    = "/v1/systemone"
	requestIDHeader  = "X-Typesafe-Request-Id"
	userAgent        = "dev-inbox-jev"
	maxResponseBytes = 4 << 20
	maxRetryAfter    = time.Minute
	maxErrorBody     = 200
)

// Request is the body of POST /v1/systemone. State and instructions are text or any JSON value; the
// question names are not shown to the model, they only key the answers.
type Request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// Question is a Choice question, the only question type dev-inbox asks.
type Question struct {
	Type         string   `json:"type"`
	Instructions any      `json:"instructions,omitempty"`
	Criteria     Criteria `json:"criteria"`
}

// Choice returns a question that selects one of the options in criteria.
func Choice(instructions any, criteria Criteria) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: criteria}
}

// Criteria are the options of a Choice question, encoded as a JSON object in this order.
type Criteria []Option

// Option is a Choice option; Description is text, any JSON value, or nil for none.
type Option struct {
	Label       string
	Description any
}

func (c Criteria) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, o := range c {
		if i > 0 {
			b.WriteByte(',')
		}
		label, err := json.Marshal(o.Label)
		if err != nil {
			return nil, err
		}
		desc, err := json.Marshal(o.Description)
		if err != nil {
			return nil, err
		}
		b.Write(label)
		b.WriteByte(':')
		b.Write(desc)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// Response holds one answer per question, keyed by the question names of the request.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

// Answer is the answer to a Choice question.
type Answer struct {
	Type   string `json:"type"`
	Choice string `json:"choice"`
	// Confidence is derived from Probabilities; see https://docs.typesafe.ai/confidence.
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// APIError is a non-2xx response.
type APIError struct {
	StatusCode int
	// Message comes from the error body: "error", "error.message", "message", "detail" or the
	// validation errors in "detail", otherwise the start of the raw body.
	Message   string
	RequestID string
	// RetryAfter is the delay the server asked for in retry-after-ms or Retry-After; zero if none.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("jev: %d %s", e.StatusCode, e.Message)
	if e.RequestID != "" {
		s += " (request " + e.RequestID + ")"
	}
	return s
}

// Retryable reports whether the SDKs retry the status: 408, 429 and 5xx.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests ||
		e.StatusCode >= 500
}

// transportError is a failed round trip without a response, which is retried.
type transportError struct{ err error }

func (e *transportError) Error() string { return "jev: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

type Config struct {
	APIKey  string        // required
	BaseURL string        // default DefaultBaseURL
	Model   string        // default DefaultModel
	Timeout time.Duration // per attempt, default DefaultTimeout
	// MaxRetries is the number of retries after the first attempt; 0 disables retries.
	MaxRetries int
	HTTPClient *http.Client // default a client with HTTP/2 health checks
	Logger     *slog.Logger // default slog.Default()
}

type Client struct {
	apiKey     string
	url        string
	model      string
	timeout    time.Duration
	maxRetries int
	http       *http.Client
	log        *slog.Logger

	backoffBase time.Duration
	backoffMax  time.Duration
}

func New(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("jev: API key is required")
	}
	c := &Client{
		apiKey:      cfg.APIKey,
		url:         strings.TrimRight(orDefault(cfg.BaseURL, DefaultBaseURL), "/") + systemOnePath,
		model:       orDefault(cfg.Model, DefaultModel),
		timeout:     cfg.Timeout,
		maxRetries:  max(cfg.MaxRetries, 0),
		http:        cfg.HTTPClient,
		log:         cfg.Logger,
		backoffBase: 500 * time.Millisecond,
		backoffMax:  5 * time.Second,
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.http == nil {
		c.http = &http.Client{Transport: httpx.Transport()}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// SystemOne answers the questions of req about its state, using the client's model unless req names
// one. Every attempt is logged as JSON-friendly attributes: the request body, then the response status,
// latency and body, or the transport error; retries and the final failure are logged too. attrs, such
// as the PR and comment IDs, are added to each of these lines. The API key is never logged.
func (c *Client) SystemOne(ctx context.Context, req Request, attrs ...slog.Attr) (*Response, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("jev: encode request: %w", err)
	}
	args := make([]any, len(attrs))
	for i, a := range attrs {
		args[i] = a
	}
	log := c.log.With(args...)
	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, log, body, attempt)
		if err == nil {
			return resp, nil
		}
		var apiErr *APIError
		var tErr *transportError
		retryable := errors.As(err, &tErr) || errors.As(err, &apiErr) && apiErr.Retryable()
		if !retryable || attempt >= c.maxRetries || ctx.Err() != nil {
			log.Error("jev request failed", "attempts", attempt+1, "err", err)
			return nil, err
		}
		delay := c.retryDelay(attempt, apiErr)
		log.Warn("jev request retry", "attempt", attempt+1, "retry_in_ms", delay.Milliseconds(), "err", err)
		select {
		case <-ctx.Done():
			log.Error("jev request failed", "attempts", attempt+1, "err", ctx.Err())
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (c *Client) attempt(ctx context.Context, log *slog.Logger, body []byte, attempt int) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("jev: %w", err)
	}
	hreq.Header.Set("Authorization", "Bearer "+c.apiKey)
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/json")
	hreq.Header.Set("User-Agent", userAgent)
	if attempt > 0 {
		hreq.Header.Set("X-TypeSafe-Retry-Count", strconv.Itoa(attempt))
	}

	log.Info("jev request", "attempt", attempt+1, "url", c.url, "body", jsonOrText(body))
	start := time.Now()
	hresp, err := c.http.Do(hreq)
	if err != nil {
		log.Warn("jev request error", "attempt", attempt+1, "latency_ms", time.Since(start).Milliseconds(), "err", err)
		return nil, &transportError{err}
	}
	defer func() { _ = hresp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes))
	latency := time.Since(start)
	if err != nil {
		log.Warn("jev request error", "attempt", attempt+1, "status", hresp.StatusCode,
			"latency_ms", latency.Milliseconds(), "err", err)
		return nil, &transportError{fmt.Errorf("read response: %w", err)}
	}
	requestID := hresp.Header.Get(requestIDHeader)
	log.Info("jev response", "attempt", attempt+1, "status", hresp.StatusCode, "latency_ms", latency.Milliseconds(),
		"request_id", requestID, "body", jsonOrText(data))

	if hresp.StatusCode < 200 || hresp.StatusCode > 299 {
		return nil, &APIError{
			StatusCode: hresp.StatusCode,
			Message:    errorMessage(data),
			RequestID:  requestID,
			RetryAfter: retryAfter(hresp.Header, time.Now()),
		}
	}
	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("jev: decode response: %w", err)
	}
	return &resp, nil
}

// retryDelay is the server's Retry-After if allowed, otherwise capped exponential backoff with jitter.
func (c *Client) retryDelay(attempt int, apiErr *APIError) time.Duration {
	if apiErr != nil && apiErr.RetryAfter > 0 && apiErr.RetryAfter <= maxRetryAfter {
		return apiErr.RetryAfter
	}
	d := min(c.backoffBase<<attempt, c.backoffMax)
	return time.Duration(float64(d) * (1 - 0.25*rand.Float64()))
}

// retryAfter reads retry-after-ms, then Retry-After in seconds or as an HTTP date.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Retry-After-Ms")), 64); err == nil && v >= 0 {
		return time.Duration(v * float64(time.Millisecond))
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if v, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(max(v, 0) * float64(time.Second))
	}
	if t, err := http.ParseTime(raw); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

// errorMessage extracts the message of an error body like the SDKs do.
func errorMessage(body []byte) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message json.RawMessage `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &v) == nil {
		var s string
		var obj struct {
			Message string `json:"message"`
		}
		var validation []struct {
			Loc []any  `json:"loc"`
			Msg string `json:"msg"`
		}
		switch {
		case json.Unmarshal(v.Error, &s) == nil && s != "":
			return s
		case json.Unmarshal(v.Error, &obj) == nil && obj.Message != "":
			return obj.Message
		case json.Unmarshal(v.Message, &s) == nil && s != "":
			return s
		case json.Unmarshal(v.Detail, &s) == nil && s != "":
			return s
		case json.Unmarshal(v.Detail, &obj) == nil && obj.Message != "":
			return obj.Message
		case json.Unmarshal(v.Detail, &validation) == nil && len(validation) > 0:
			parts := make([]string, 0, len(validation))
			for _, e := range validation {
				var loc []string
				for _, l := range e.Loc {
					if l != "body" {
						loc = append(loc, fmt.Sprint(l))
					}
				}
				if len(loc) > 0 {
					parts = append(parts, strings.Join(loc, ".")+": "+e.Msg)
				} else {
					parts = append(parts, e.Msg)
				}
			}
			return strings.Join(parts, "; ")
		}
	}
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return "status code (no body)"
	}
	if len(raw) > maxErrorBody {
		return raw[:maxErrorBody] + "…"
	}
	return raw
}

// jsonOrText makes a slog JSON handler embed body as JSON when it is valid JSON, and as a string
// otherwise.
func jsonOrText(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return string(body)
}
