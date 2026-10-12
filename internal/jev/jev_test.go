package jev

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "ts-secret-key-0123456789"

type recorded struct {
	header http.Header
	path   string
	method string
	body   []byte
}

// fakeAPI serves the given responses in order, repeating the last one, and records each request.
type fakeAPI struct {
	mu        sync.Mutex
	requests  []recorded
	responses []func(w http.ResponseWriter)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	n := len(f.requests)
	f.requests = append(f.requests, recorded{header: r.Header.Clone(), path: r.URL.Path, method: r.Method, body: body})
	respond := f.responses[min(n, len(f.responses)-1)]
	f.mu.Unlock()
	respond(w)
}

func (f *fakeAPI) recorded() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func status(code int, headers map[string]string, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

const okBody = `{"model":"jev-1.13.0","answers":{"c1":{"type":"choice","choice":"ack","confidence":0.8,` +
	`"probabilities":{"question_for_me":0.05,"ack":0.9,"not_for_me":0.05}}},"usage":{"input_tokens":300,"output_tokens":20}}`

func newTestClient(t *testing.T, api *fakeAPI, maxRetries int) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	c, err := New(Config{
		APIKey:     testKey,
		BaseURL:    srv.URL + "/",
		MaxRetries: maxRetries,
		Logger:     slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.backoffBase = time.Millisecond
	c.backoffMax = 5 * time.Millisecond
	return c, &logs
}

func testRequest() Request {
	return Request{
		State: map[string]any{"viewer": "me"},
		Questions: map[string]Question{
			"c1": Choice(map[string]any{"question": "What does `comment` want?"}, Criteria{
				{Label: "question_for_me", Description: "Asks a question"},
				{Label: "ack", Description: "Thanks only"},
				{Label: "not_for_me", Description: nil},
			}),
		},
	}
}

// logLines decodes the JSON lines written by the client's logger.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(logs.Bytes()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %s", sc.Text())
		}
		lines = append(lines, m)
	}
	return lines
}

func TestSystemOneWireFormat(t *testing.T) {
	api := &fakeAPI{responses: []func(http.ResponseWriter){
		status(200, map[string]string{"Content-Type": "application/json", "X-Typesafe-Request-Id": "req-1"}, okBody),
	}}
	c, _ := newTestClient(t, api, 0)

	resp, err := c.SystemOne(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}

	reqs := api.recorded()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.method != http.MethodPost || r.path != "/v1/systemone" {
		t.Errorf("request %s %s, want POST /v1/systemone", r.method, r.path)
	}
	for name, want := range map[string]string{
		"Authorization": "Bearer " + testKey,
		"Content-Type":  "application/json",
		"Accept":        "application/json",
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	if got := r.header.Get("X-TypeSafe-Retry-Count"); got != "" {
		t.Errorf("first attempt carries retry count %q", got)
	}
	want := `{"state":{"viewer":"me"},"model":"jev-latest","questions":{"c1":{"type":"choice",` +
		`"instructions":{"question":"What does ` + "`comment`" + ` want?"},` +
		`"criteria":{"question_for_me":"Asks a question","ack":"Thanks only","not_for_me":null}}}}`
	if string(r.body) != want {
		t.Errorf("body\n got %s\nwant %s", r.body, want)
	}

	a := resp.Answers["c1"]
	if resp.Model != "jev-1.13.0" || a.Type != "choice" || a.Choice != "ack" || a.Confidence != 0.8 ||
		a.Probabilities["ack"] != 0.9 || resp.Usage.InputTokens != 300 || resp.Usage.OutputTokens != 20 {
		t.Errorf("decoded response %+v", resp)
	}
}

func TestSystemOneRetriesRateLimit(t *testing.T) {
	api := &fakeAPI{responses: []func(http.ResponseWriter){
		status(429, map[string]string{"Retry-After-Ms": "1"}, `{"error":"rate limited"}`),
		status(529, nil, `{"error":{"message":"overloaded"}}`),
		status(200, nil, okBody),
	}}
	c, logs := newTestClient(t, api, 2)

	if _, err := c.SystemOne(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	reqs := api.recorded()
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3", len(reqs))
	}
	for i, r := range reqs[1:] {
		if got, want := r.header.Get("X-TypeSafe-Retry-Count"), []string{"1", "2"}[i]; got != want {
			t.Errorf("retry %d carries retry count %q, want %q", i+1, got, want)
		}
	}
	var retries int
	for _, l := range logLines(t, logs) {
		if l["msg"] == "jev request retry" {
			retries++
		}
	}
	if retries != 2 {
		t.Errorf("logged %d retries, want 2", retries)
	}
}

func TestSystemOneErrors(t *testing.T) {
	tests := []struct {
		name      string
		resp      func(http.ResponseWriter)
		retries   int
		attempts  int
		status    int
		message   string
		retryable bool
	}{
		{
			name:      "rate limit exhausts retries",
			resp:      status(429, map[string]string{"Retry-After": "0"}, `{"error":"slow down"}`),
			retries:   2,
			attempts:  3,
			status:    429,
			message:   "slow down",
			retryable: true,
		},
		{
			name: "validation error is not retried",
			resp: status(422, nil,
				`{"detail":[{"loc":["body","questions","c1","criteria"],"msg":"Field required","type":"missing"}]}`),
			retries:  2,
			attempts: 1,
			status:   422,
			message:  "questions.c1.criteria: Field required",
		},
		{
			name:     "unauthorized",
			resp:     status(401, nil, `{"detail":"Invalid API key"}`),
			retries:  2,
			attempts: 1,
			status:   401,
			message:  "Invalid API key",
		},
		{
			name:      "server error without JSON body",
			resp:      status(503, nil, "upstream unavailable"),
			retries:   0,
			attempts:  1,
			status:    503,
			message:   "upstream unavailable",
			retryable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{responses: []func(http.ResponseWriter){tt.resp}}
			c, logs := newTestClient(t, api, tt.retries)

			_, err := c.SystemOne(context.Background(), testRequest())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("got error %v, want *APIError", err)
			}
			if apiErr.StatusCode != tt.status || apiErr.Message != tt.message || apiErr.Retryable() != tt.retryable {
				t.Errorf("got %+v (retryable %v), want status %d, message %q, retryable %v",
					apiErr, apiErr.Retryable(), tt.status, tt.message, tt.retryable)
			}
			if n := len(api.recorded()); n != tt.attempts {
				t.Errorf("got %d attempts, want %d", n, tt.attempts)
			}
			lines := logLines(t, logs)
			if last := lines[len(lines)-1]; last["msg"] != "jev request failed" || last["level"] != "ERROR" {
				t.Errorf("last log line %v, want the final failure", last)
			}
		})
	}
}

func TestSystemOneRetriesTransportErrors(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			// Drop the connection without a response.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, okBody)
	}))
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	c, err := New(Config{APIKey: testKey, BaseURL: srv.URL, MaxRetries: 1, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	c.backoffBase = time.Millisecond

	if _, err := c.SystemOne(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("got %d calls, want 2", calls)
	}
	if !strings.Contains(logs.String(), `"msg":"jev request error"`) {
		t.Errorf("transport error not logged:\n%s", logs.String())
	}
}

func TestSystemOneLogsRequestAndResponseWithoutKey(t *testing.T) {
	api := &fakeAPI{responses: []func(http.ResponseWriter){
		status(500, nil, `{"error":"boom"}`),
		status(200, map[string]string{"X-Typesafe-Request-Id": "req-2"}, okBody),
	}}
	c, logs := newTestClient(t, api, 1)

	if _, err := c.SystemOne(context.Background(), testRequest(),
		slog.String("pr", "acme/api#1"), slog.Any("comment_ids", []string{"c1"})); err != nil {
		t.Fatal(err)
	}

	raw := logs.String()
	if strings.Contains(raw, testKey) || strings.Contains(raw, "Bearer") || strings.Contains(raw, "Authorization") {
		t.Fatalf("log leaks the API key or auth header:\n%s", raw)
	}
	var requests, responses []map[string]any
	for _, l := range logLines(t, logs) {
		if l["pr"] != "acme/api#1" || !strings.Contains(toJSON(t, l["comment_ids"]), `"c1"`) {
			t.Errorf("log line without PR and comment IDs: %v", l)
		}
		switch l["msg"] {
		case "jev request":
			requests = append(requests, l)
		case "jev response":
			responses = append(responses, l)
		}
	}
	if len(requests) != 2 || len(responses) != 2 {
		t.Fatalf("logged %d requests and %d responses, want 2 each:\n%s", len(requests), len(responses), raw)
	}
	// The body is embedded as JSON, not as an escaped string.
	body, ok := requests[0]["body"].(map[string]any)
	if !ok || body["model"] != "jev-latest" || body["state"] == nil || body["questions"] == nil {
		t.Errorf("request body logged as %v", requests[0]["body"])
	}
	if responses[0]["status"] != float64(500) || toJSON(t, responses[0]["body"]) != `{"error":"boom"}` {
		t.Errorf("error response logged as %v", responses[0])
	}
	ok200 := responses[1]
	if ok200["status"] != float64(200) || ok200["request_id"] != "req-2" || ok200["latency_ms"] == nil {
		t.Errorf("response logged as %v", ok200)
	}
	if answers, _ := ok200["body"].(map[string]any)["answers"].(map[string]any); answers["c1"] == nil {
		t.Errorf("response body logged as %v", ok200["body"])
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without an API key succeeded")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		headers map[string]string
		want    time.Duration
	}{
		{nil, 0},
		{map[string]string{"Retry-After-Ms": "1500", "Retry-After": "9"}, 1500 * time.Millisecond},
		{map[string]string{"Retry-After": "2"}, 2 * time.Second},
		{map[string]string{"Retry-After": now.Add(3 * time.Second).Format(http.TimeFormat)}, 3 * time.Second},
		{map[string]string{"Retry-After": "soon"}, 0},
	}
	for _, tt := range tests {
		h := http.Header{}
		for k, v := range tt.headers {
			h.Set(k, v)
		}
		if got := retryAfter(h, now); got != tt.want {
			t.Errorf("retryAfter(%v) = %s, want %s", tt.headers, got, tt.want)
		}
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
