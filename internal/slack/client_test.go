package slack

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func authTestHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "user_id": "U0VIEWER", "url": "https://acme-co.slack.com/"})
}

func TestAuthTest(t *testing.T) {
	f := newFake(t)
	f.handle("auth.test", authTestHandler)
	c, _ := f.newClient(t, f.validToken())
	got, err := c.AuthTest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := (Identity{UserID: "U0VIEWER", TeamURL: "https://acme-co.slack.com/"}); got != want {
		t.Errorf("AuthTest = %+v, want %+v", got, want)
	}
}

func TestClientTokenRefresh(t *testing.T) {
	tests := []struct {
		name          string
		token         func(f *fakeSlack) Token
		setup         func(f *fakeSlack)
		wantRefreshes int
		wantCode      string // APIError code; empty for success
		wantErr       string // other error substring
	}{
		{
			name:  "valid token is used as is",
			token: func(f *fakeSlack) Token { return f.validToken() },
		},
		{
			name: "near expiry refreshes before the call",
			token: func(f *fakeSlack) Token {
				t := f.validToken()
				t.ExpiresAt = time.Now().Add(2 * time.Minute)
				return t
			},
			wantRefreshes: 1,
		},
		{
			name:          "token_expired refreshes and retries",
			token:         func(f *fakeSlack) Token { return f.validToken() },
			setup:         func(f *fakeSlack) { f.expired[f.access] = true },
			wantRefreshes: 1,
		},
		{
			name: "invalid_auth on a rotating token refreshes and retries",
			token: func(f *fakeSlack) Token {
				t := f.validToken()
				t.AccessToken = "xoxe.xoxp-1-revoked"
				return t
			},
			wantRefreshes: 1,
		},
		{
			name: "invalid_auth on a non-expiring token fails",
			token: func(f *fakeSlack) Token {
				return Token{AccessToken: "xoxp-1-revoked"}
			},
			wantCode: "invalid_auth",
		},
		{
			name:          "refreshes only once per call",
			token:         func(f *fakeSlack) Token { return f.validToken() },
			setup:         func(f *fakeSlack) { f.rejectAll = "token_expired" },
			wantRefreshes: 1,
			wantCode:      "token_expired",
		},
		{
			name: "failed refresh hints at login",
			token: func(f *fakeSlack) Token {
				t := f.validToken()
				t.RefreshToken = "xoxe-1-refresh-spent"
				t.ExpiresAt = time.Now().Add(time.Minute)
				return t
			},
			wantErr: "dev-inbox-slack login",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			f.handle("auth.test", authTestHandler)
			tok := tt.token(f)
			if tt.setup != nil {
				f.mu.Lock()
				tt.setup(f)
				f.mu.Unlock()
			}
			c, _ := f.newClient(t, tok)

			_, err := c.AuthTest(t.Context())
			switch {
			case tt.wantCode != "":
				if apiErr, ok := errors.AsType[*APIError](err); !ok || apiErr.Code != tt.wantCode {
					t.Fatalf("AuthTest error = %v, want code %s", err, tt.wantCode)
				}
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("AuthTest error = %v, want it to contain %q", err, tt.wantErr)
				}
				for _, secret := range []string{tok.AccessToken, tok.RefreshToken, testClientID} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error %q leaks %q", err, secret)
					}
				}
			case err != nil:
				t.Fatalf("AuthTest: %v", err)
			}

			access, refresh, n := f.tokens()
			if n != tt.wantRefreshes {
				t.Errorf("refreshes = %d, want %d", n, tt.wantRefreshes)
			}
			if tt.wantRefreshes == 0 {
				return
			}
			stored, err := c.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if stored.AccessToken != access || stored.RefreshToken != refresh {
				t.Errorf("stored token = %+v, want rotated access %q refresh %q", stored, access, refresh)
			}
			if time.Until(stored.ExpiresAt) < 11*time.Hour {
				t.Errorf("stored ExpiresAt = %v, want about 12h from now", stored.ExpiresAt)
			}
		})
	}
}

func TestClientConcurrentRefreshOnce(t *testing.T) {
	f := newFake(t)
	f.handle("auth.test", authTestHandler)
	tok := f.validToken()
	tok.ExpiresAt = time.Now().Add(time.Minute)
	c, _ := f.newClient(t, tok)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := c.AuthTest(t.Context())
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if _, _, n := f.tokens(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
}

func TestClientRateLimit(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter []string // one 429 per entry, then success
		cancel     bool
		wantWaits  []time.Duration
		wantErr    error
	}{
		{name: "waits Retry-After", retryAfter: []string{"2", "1"}, wantWaits: []time.Duration{2 * time.Second, time.Second}},
		{name: "missing Retry-After waits a second", retryAfter: []string{""}, wantWaits: []time.Duration{time.Second}},
		{name: "canceled while waiting", retryAfter: []string{"30"}, cancel: true, wantWaits: []time.Duration{30 * time.Second}, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			var mu sync.Mutex
			pending := tt.retryAfter
			f.handle("auth.test", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if len(pending) > 0 {
					if pending[0] != "" {
						w.Header().Set("Retry-After", pending[0])
					}
					pending = pending[1:]
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				authTestHandler(w, r)
			})
			c, waits := f.newClient(t, f.validToken())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				wait := c.wait
				c.wait = func(ctx context.Context, d time.Duration) error {
					cancel()
					return wait(ctx, d)
				}
			}

			_, err := c.AuthTest(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("AuthTest error = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(*waits, tt.wantWaits) {
				t.Errorf("waits = %v, want %v", *waits, tt.wantWaits)
			}
		})
	}
}

func TestClientRateLimitGivesUp(t *testing.T) {
	f := newFake(t)
	f.handle("auth.test", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, waits := f.newClient(t, f.validToken())
	if _, err := c.AuthTest(t.Context()); err == nil {
		t.Fatal("AuthTest succeeded under a permanent rate limit")
	}
	if len(*waits) != maxRateLimitRetries {
		t.Errorf("waited %d times, want %d", len(*waits), maxRateLimitRetries)
	}
}

func TestSleep(t *testing.T) {
	if err := sleep(t.Context(), time.Millisecond); err != nil {
		t.Errorf("sleep = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep on canceled context = %v, want Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Error("sleep ignored the canceled context")
	}
}

// pages serves pages[cursor] and links each page to the next as cursor "c1", "c2", ...
func pages(t *testing.T, key string, pages ...[]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		i := 0
		if cur := r.URL.Query().Get("cursor"); cur != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(cur, "c"))
			if err != nil || n <= 0 || n >= len(pages) {
				t.Errorf("unexpected cursor %q", cur)
				return
			}
			i = n
		}
		next := ""
		if i+1 < len(pages) {
			next = "c" + strconv.Itoa(i+1)
		}
		writeJSON(w, map[string]any{"ok": true, key: pages[i], "has_more": next != "", "response_metadata": map[string]any{"next_cursor": next}})
	}
}

func TestHistoryPagination(t *testing.T) {
	f := newFake(t)
	f.handle("conversations.history", pages(t, "messages",
		[]map[string]any{
			{"ts": "1760000300.000000", "user": "U1", "text": "<@U0VIEWER> look", "thread_ts": "1760000300.000000", "reply_count": 2, "latest_reply": "1760000400.000000"},
			{"ts": "1760000200.000000", "bot_id": "B1", "subtype": "bot_message", "text": "deploy"},
		},
		[]map[string]any{{"ts": "1760000100.000000", "user": "U2", "text": "hi"}},
		[]map[string]any{{"ts": "1760000050.000000", "user": "U3", "text": "old"}},
	))
	c, _ := f.newClient(t, f.validToken())

	got, err := c.History(t.Context(), "C0123ABCD", time.UnixMicro(1760000000000100))
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{
		{TS: "1760000300.000000", ThreadTS: "1760000300.000000", User: "U1", Text: "<@U0VIEWER> look", ReplyCount: 2, LatestReply: "1760000400.000000"},
		{TS: "1760000200.000000", BotID: "B1", Subtype: "bot_message", Text: "deploy"},
		{TS: "1760000100.000000", User: "U2", Text: "hi"},
		{TS: "1760000050.000000", User: "U3", Text: "old"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("History =\n%+v\nwant\n%+v", got, want)
	}
	calls := f.callsOf("conversations.history")
	if len(calls) != 3 {
		t.Fatalf("history calls = %d, want 3", len(calls))
	}
	for i, q := range calls {
		wantCursor := []string{"", "c1", "c2"}[i]
		if q.Get("channel") != "C0123ABCD" || q.Get("oldest") != "1760000000.000100" || q.Get("limit") != "200" || q.Get("cursor") != wantCursor {
			t.Errorf("call %d query = %v, want channel C0123ABCD, oldest 1760000000.000100, limit 200, cursor %q", i, q, wantCursor)
		}
	}
}

func TestRepliesPagination(t *testing.T) {
	f := newFake(t)
	f.handle("conversations.replies", pages(t, "messages",
		[]map[string]any{
			{"ts": "1760000300.000000", "thread_ts": "1760000300.000000", "user": "U1", "text": "parent", "reply_count": 2},
			{"ts": "1760000350.000000", "thread_ts": "1760000300.000000", "user": "U2", "text": "first"},
		},
		[]map[string]any{{"ts": "1760000400.000000", "thread_ts": "1760000300.000000", "user": "U3", "text": "second"}},
	))
	c, _ := f.newClient(t, f.validToken())

	got, err := c.Replies(t.Context(), "C0123ABCD", "1760000300.000000")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range got {
		texts = append(texts, m.Text)
	}
	if want := []string{"parent", "first", "second"}; !reflect.DeepEqual(texts, want) {
		t.Errorf("Replies texts = %v, want %v", texts, want)
	}
	calls := f.callsOf("conversations.replies")
	if len(calls) != 2 || calls[0].Get("ts") != "1760000300.000000" || calls[0].Get("limit") != "200" || calls[1].Get("cursor") != "c1" {
		t.Errorf("replies calls = %v", calls)
	}
}

func TestChannel(t *testing.T) {
	tests := []struct {
		in         string
		want       Channel
		wantMethod string
		wantCalls  int
		wantErr    bool
	}{
		{in: "C0123ABCD", want: Channel{ID: "C0123ABCD", Name: "dev-pr-reviews"}, wantMethod: "conversations.info", wantCalls: 1},
		{in: "G0PRIVATE1", want: Channel{ID: "G0PRIVATE1", Name: "secret"}, wantMethod: "conversations.info", wantCalls: 1},
		{in: "#dev-pr-reviews", want: Channel{ID: "C0123ABCD", Name: "dev-pr-reviews"}, wantMethod: "conversations.list", wantCalls: 2},
		{in: "dev-pr-reviews", want: Channel{ID: "C0123ABCD", Name: "dev-pr-reviews"}, wantMethod: "conversations.list", wantCalls: 2},
		{in: "general", want: Channel{ID: "C0GENERAL", Name: "general"}, wantMethod: "conversations.list", wantCalls: 1},
		{in: "#missing", wantMethod: "conversations.list", wantCalls: 2, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			f := newFake(t)
			f.handle("conversations.info", func(w http.ResponseWriter, r *http.Request) {
				names := map[string]string{"C0123ABCD": "dev-pr-reviews", "G0PRIVATE1": "secret"}
				id := r.URL.Query().Get("channel")
				writeJSON(w, map[string]any{"ok": true, "channel": map[string]any{"id": id, "name": names[id], "is_channel": true}})
			})
			f.handle("conversations.list", pages(t, "channels",
				[]map[string]any{{"id": "C0GENERAL", "name": "general"}, {"id": "C0RANDOM1", "name": "random"}},
				[]map[string]any{{"id": "C0123ABCD", "name": "dev-pr-reviews"}},
			))
			c, _ := f.newClient(t, f.validToken())

			got, err := c.Channel(t.Context(), tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Channel(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Channel(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
			calls := f.callsOf(tt.wantMethod)
			if len(calls) != tt.wantCalls {
				t.Fatalf("%s calls = %d, want %d", tt.wantMethod, len(calls), tt.wantCalls)
			}
			if tt.wantMethod == "conversations.list" {
				q := calls[0]
				if q.Get("types") != "public_channel,private_channel" || q.Get("exclude_archived") != "true" || q.Get("limit") != "1000" {
					t.Errorf("conversations.list query = %v", q)
				}
			}
		})
	}
}

func TestUserName(t *testing.T) {
	tests := []struct {
		name    string
		user    map[string]any
		apiErr  string
		want    string
		wantErr bool
	}{
		{name: "display name", user: map[string]any{"name": "alice.a", "profile": map[string]any{"display_name": "alice", "real_name": "Alice A"}}, want: "alice"},
		{name: "real name fallback", user: map[string]any{"name": "bob.b", "profile": map[string]any{"display_name": "", "real_name": "Bob B"}}, want: "Bob B"},
		{name: "user name fallback", user: map[string]any{"name": "carol", "profile": map[string]any{}}, want: "carol"},
		{name: "ID fallback", user: map[string]any{"profile": map[string]any{}}, want: "U0TEST"},
		{name: "error is not cached", apiErr: "user_not_found", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			f.handle("users.info", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("user") != "U0TEST" {
					t.Errorf("users.info user = %q", r.URL.Query().Get("user"))
				}
				if tt.apiErr != "" {
					writeJSON(w, map[string]any{"ok": false, "error": tt.apiErr})
					return
				}
				writeJSON(w, map[string]any{"ok": true, "user": tt.user})
			})
			c, _ := f.newClient(t, f.validToken())

			for range 2 {
				got, err := c.UserName(t.Context(), "U0TEST")
				if (err != nil) != tt.wantErr || got != tt.want {
					t.Fatalf("UserName = %q, %v; want %q, wantErr %v", got, err, tt.want, tt.wantErr)
				}
			}
			wantCalls := 1
			if tt.wantErr {
				wantCalls = 2
			}
			if n := len(f.callsOf("users.info")); n != wantCalls {
				t.Errorf("users.info calls = %d, want %d", n, wantCalls)
			}
		})
	}
}

func TestAPIError(t *testing.T) {
	f := newFake(t)
	f.handle("conversations.info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"ok": false, "error": "channel_not_found"})
	})
	c, _ := f.newClient(t, f.validToken())
	_, err := c.Channel(t.Context(), "C0NOPE0001")
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok || apiErr.Method != "conversations.info" || apiErr.Code != "channel_not_found" {
		t.Errorf("error = %v, want APIError conversations.info channel_not_found", err)
	}
}

func TestFormatTS(t *testing.T) {
	tests := []struct {
		in   time.Time
		want string
	}{
		{time.UnixMicro(1760000000000100), "1760000000.000100"},
		{time.Unix(1760000000, 0), "1760000000.000000"},
		{time.Unix(1760000000, 999_999_999), "1760000000.999999"},
		{time.UnixMicro(-1), "-1.999999"},
	}
	for _, tt := range tests {
		if got := formatTS(tt.in); got != tt.want {
			t.Errorf("formatTS(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"3", 3 * time.Second},
		{" 0 ", 0},
		{"", time.Second},
		{"soon", time.Second},
		{"-5", time.Second},
	}
	for _, tt := range tests {
		if got := retryAfter(tt.in); got != tt.want {
			t.Errorf("retryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
