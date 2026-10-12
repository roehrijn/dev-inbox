package github

import (
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestTeamMembers(t *testing.T) {
	var requests atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeGQL(t, r)
		if !strings.HasPrefix(req.Query, "query TeamMembers(") {
			t.Errorf("unexpected query %q", req.Query)
		}
		if strings.Contains(req.Query, "acme-co") || strings.Contains(req.Query, "control-plane") {
			t.Errorf("query contains spliced values: %q", req.Query)
		}
		switch n := requests.Add(1); n {
		case 1:
			want := map[string]any{"org": "acme-co", "slug": "control-plane"}
			if !reflect.DeepEqual(req.Variables, want) {
				t.Errorf("page 1 variables = %v, want %v", req.Variables, want)
			}
			_, _ = w.Write(readFixture(t, "team_members_page1.json"))
		case 2:
			want := map[string]any{"org": "acme-co", "slug": "control-plane", "cursor": "Y3Vyc29yOjI="}
			if !reflect.DeepEqual(req.Variables, want) {
				t.Errorf("page 2 variables = %v, want %v", req.Variables, want)
			}
			_, _ = w.Write(readFixture(t, "team_members_page2.json"))
		default:
			t.Errorf("unexpected request %d", n)
		}
	}))

	got, err := c.TeamMembers(t.Context(), "acme-co/control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alice", "bob", "carol"}; !reflect.DeepEqual(got, want) {
		t.Errorf("TeamMembers = %v, want %v", got, want)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("requests = %d, want 2", n)
	}
}

func TestTeamMembersEmptyTeam(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": {"organization": {"team": {"members": {"nodes": [], "pageInfo": {"hasNextPage": false, "endCursor": null}}}}}}`))
	}))
	got, err := c.TeamMembers(t.Context(), "o/empty")
	if err != nil || len(got) != 0 {
		t.Errorf("TeamMembers = %v, %v, want no members", got, err)
	}
}

func TestTeamMembersErrors(t *testing.T) {
	const page1 = `{"data": {"organization": {"team": {"members": {"nodes": [{"login": "alice"}], "pageInfo": {"hasNextPage": true, "endCursor": "c1"}}}}}}`
	tests := []struct {
		name         string
		bodies       []string // one response per request
		wantNotFound bool
		wantGraphQL  bool // err wraps *api.GraphQLError
	}{
		{
			name:         "unknown team",
			bodies:       []string{`{"data": {"organization": {"team": null}}, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to a Team with the slug 'nope'.", "path": ["organization", "team"]}]}`},
			wantNotFound: true,
			wantGraphQL:  true,
		},
		{
			name:         "null team without errors",
			bodies:       []string{`{"data": {"organization": {"team": null}}}`},
			wantNotFound: true,
		},
		{
			name:         "unknown organization",
			bodies:       []string{`{"data": {"organization": null}, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to an Organization with the login of 'nope'.", "path": ["organization"]}]}`},
			wantNotFound: true,
			wantGraphQL:  true,
		},
		{
			name:         "null organization without errors",
			bodies:       []string{`{"data": {"organization": null}}`},
			wantNotFound: true,
		},
		{
			name:         "organization behind SAML enforcement",
			bodies:       []string{`{"data": {"organization": null}, "errors": [{"type": "FORBIDDEN", "message": "Resource protected by organization SAML enforcement", "path": ["organization"]}]}`},
			wantNotFound: true,
			wantGraphQL:  true,
		},
		{
			name:         "no data at all",
			bodies:       []string{`{"data": null, "errors": [{"type": "FORBIDDEN", "message": "Resource protected by organization SAML enforcement"}]}`},
			wantNotFound: true,
			wantGraphQL:  true,
		},
		{
			name:         "forbidden on a later page",
			bodies:       []string{page1, `{"data": {"organization": null}, "errors": [{"type": "FORBIDDEN", "message": "Resource protected by organization SAML enforcement", "path": ["organization"]}]}`},
			wantNotFound: true,
			wantGraphQL:  true,
		},
		{
			name:        "transient partial error is rejected",
			bodies:      []string{`{"data": {"organization": {"team": {"members": {"nodes": [{"login": "alice"}], "pageInfo": {"hasNextPage": false}}}}}, "errors": [{"message": "Something went wrong while executing your query.", "path": ["organization", "team", "members"]}]}`},
			wantGraphQL: true,
		},
		{
			name:        "query error",
			bodies:      []string{`{"errors": [{"message": "Field 'bogus' doesn't exist on type 'Team'"}]}`},
			wantGraphQL: true,
		},
		{
			name:   "next page without cursor",
			bodies: []string{`{"data": {"organization": {"team": {"members": {"nodes": [{"login": "alice"}], "pageInfo": {"hasNextPage": true, "endCursor": ""}}}}}}`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(requests.Add(1)) - 1
				if i >= len(tt.bodies) {
					t.Errorf("unexpected request %d", i+1)
					return
				}
				_, _ = w.Write([]byte(tt.bodies[i]))
			}))
			got, err := c.TeamMembers(t.Context(), "o/team")
			if err == nil {
				t.Fatalf("TeamMembers = %v, want error", got)
			}
			if got != nil {
				t.Errorf("TeamMembers = %v alongside error, want nil", got)
			}
			if is := errors.Is(err, ErrNotFound); is != tt.wantNotFound {
				t.Errorf("errors.Is(%v, ErrNotFound) = %t, want %t", err, is, tt.wantNotFound)
			}
			var gerr *api.GraphQLError
			if is := errors.As(err, &gerr); is != tt.wantGraphQL {
				t.Errorf("errors.As(%v, *api.GraphQLError) = %t, want %t", err, is, tt.wantGraphQL)
			}
			if n := int(requests.Load()); n != len(tt.bodies) {
				t.Errorf("requests = %d, want %d", n, len(tt.bodies))
			}
		})
	}
}

func TestTeamMembersMalformedTeam(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	for _, team := range []string{"", "acme-co", "acme-co/", "/control-plane", "/", "acme-co/control-plane/extra"} {
		got, err := c.TeamMembers(t.Context(), team)
		if err == nil {
			t.Errorf("TeamMembers(%q) = %v, want error", team, got)
			continue
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("TeamMembers(%q): err = %v, must not be ErrNotFound", team, err)
		}
	}
}

func TestTeamMembersRateLimit(t *testing.T) {
	reset := testNow.Add(30 * time.Minute).Truncate(time.Second)
	type response struct {
		status int
		header map[string]string
		body   string
	}
	ok := response{status: http.StatusOK, body: `{"data": {"organization": {"team": {"members": {"nodes": [{"login": "alice"}], "pageInfo": {"hasNextPage": true, "endCursor": "c1"}}}}}}`}
	tests := []struct {
		name      string
		responses []response
		wantReset time.Time
	}{
		{
			name: "GraphQL RATE_LIMITED",
			responses: []response{{
				status: http.StatusOK,
				header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)},
				body:   `{"data": null, "errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}]}`,
			}},
			wantReset: reset,
		},
		{
			name: "secondary limit on a later page drops partial results",
			responses: []response{ok, {
				status: http.StatusForbidden,
				header: map[string]string{"Retry-After": "60"},
				body:   `{"message": "You have exceeded a secondary rate limit."}`,
			}},
			wantReset: testNow.Add(time.Minute),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(requests.Add(1)) - 1
				if i >= len(tt.responses) {
					t.Errorf("unexpected request %d", i+1)
					return
				}
				res := tt.responses[i]
				for k, v := range res.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(res.status)
				_, _ = w.Write([]byte(res.body))
			}))
			got, err := c.TeamMembers(t.Context(), "o/team")
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want *RateLimitError", err)
			}
			if !rl.Reset.Equal(tt.wantReset) {
				t.Errorf("Reset = %s, want %s", rl.Reset, tt.wantReset)
			}
			if got != nil {
				t.Errorf("TeamMembers = %v alongside error, want nil", got)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("err = %v, must not be ErrNotFound", err)
			}
		})
	}
}
