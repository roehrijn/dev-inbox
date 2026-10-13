package slackplugin

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/slack"
)

const (
	viewerID = "UME"
	teamURL  = "https://acme-co.slack.com/"
)

var (
	groups   = []string{"SREV"}
	reviews  = slack.Channel{ID: "C1", Name: "dev-pr-reviews"}
	linkAPI  = "<https://github.com/acme/api/pull/1>"
	linkWeb  = "<https://github.com/acme/web/pull/2|acme/web#2>"
	keyAPI   = model.PRKey{Owner: "acme", Repo: "api", Number: 1}
	keyWeb   = model.PRKey{Owner: "acme", Repo: "web", Number: 2}
	parentTS = "1760000000.000100"
)

func TestToMention(t *testing.T) {
	parent := slack.Message{TS: parentTS, ThreadTS: parentTS, User: "UALICE", Text: "please review " + linkAPI,
		ReplyCount: 1, LatestReply: "1760000100.000200"}
	reply := func(text string) slack.Message {
		return slack.Message{TS: "1760000100.000200", ThreadTS: parentTS, User: "UBOB", Text: text}
	}
	msg := func(text string) slack.Message {
		return slack.Message{TS: "1760000200.000300", User: "UALICE", Text: text}
	}
	tests := []struct {
		name     string
		m        slack.Message
		parent   *slack.Message
		kind     tagKind
		prs      []model.PRKey
		threadTS string
	}{
		{name: "user mention", m: msg("<@UME> " + linkAPI), kind: tagDirect, prs: []model.PRKey{keyAPI}},
		{name: "user mention with label", m: msg("<@UME|jan> " + linkAPI), kind: tagDirect, prs: []model.PRKey{keyAPI}},
		{name: "other user", m: msg("<@UOTHER> " + linkAPI)},
		{name: "user ID prefix", m: msg("<@UME2> " + linkAPI)},
		{name: "group in groups", m: msg("<!subteam^SREV> " + linkAPI), kind: tagGroup, prs: []model.PRKey{keyAPI}},
		{name: "group with label", m: msg("<!subteam^SREV|@reviewers> " + linkAPI), kind: tagGroup, prs: []model.PRKey{keyAPI}},
		{name: "group not in groups", m: msg("<!subteam^SOTHER|@other> " + linkAPI)},
		{name: "direct and group", m: msg("<!subteam^SREV> <@UME> " + linkAPI), kind: tagDirect, prs: []model.PRKey{keyAPI}},
		{name: "here", m: msg("<!here> " + linkAPI)},
		{name: "channel", m: msg("<!channel|@channel> " + linkAPI)},
		{name: "own message", m: slack.Message{TS: "1760000200.000300", User: viewerID, Text: "<!subteam^SREV> " + linkAPI}},
		{name: "bot ID", m: slack.Message{TS: "1760000200.000300", User: "UALICE", BotID: "B1", Text: "<@UME> " + linkAPI}},
		{name: "bot message", m: slack.Message{TS: "1760000200.000300", Subtype: "bot_message", Text: "<@UME> " + linkAPI}},
		{name: "no link", m: msg("<@UME> can you help?")},
		{name: "multiple links deduplicated", m: msg("<@UME> " + linkWeb + " and " + linkAPI + " again " + linkWeb),
			kind: tagDirect, prs: []model.PRKey{keyWeb, keyAPI}},
		{name: "link with label", m: msg("<@UME> <https://github.com/acme/api/pull/1|fix it>"), kind: tagDirect,
			prs: []model.PRKey{keyAPI}},
		{name: "trailing files", m: msg("<@UME> <https://github.com/acme/api/pull/1/files>"), kind: tagDirect,
			prs: []model.PRKey{keyAPI}},
		{name: "fragment and query", m: msg("<@UME> <https://github.com/acme/api/pull/1#discussion_r1> " +
			"<https://github.com/acme/web/pull/2?w=1>"), kind: tagDirect, prs: []model.PRKey{keyAPI, keyWeb}},
		{name: "bare link", m: msg("<@UME> https://github.com/acme/api/pull/1 thanks"), kind: tagDirect,
			prs: []model.PRKey{keyAPI}},
		{name: "case as written", m: msg("<@UME> <https://GitHub.com/Acme/API.go/pull/3>"), kind: tagDirect,
			prs: []model.PRKey{{Owner: "Acme", Repo: "API.go", Number: 3}}},
		{name: "number followed by letters", m: msg("<@UME> https://github.com/acme/api/pull/12abc")},
		{name: "issue link", m: msg("<@UME> <https://github.com/acme/api/issues/1>")},
		{name: "reply inherits parent links", m: reply("<@UME> can you take this one?"), parent: &parent,
			kind: tagDirect, prs: []model.PRKey{keyAPI}, threadTS: parentTS},
		{name: "reply keeps own links", m: reply("<!subteam^SREV> " + linkWeb), parent: &parent,
			kind: tagGroup, prs: []model.PRKey{keyWeb}, threadTS: parentTS},
		{name: "untagging reply", m: reply("LGTM " + linkWeb), parent: &parent},
		{name: "reply without known parent", m: reply("<@UME> anyone?")},
		{name: "broadcast reply without known parent", m: reply("<@UME> " + linkAPI), kind: tagDirect,
			prs: []model.PRKey{keyAPI}, threadTS: parentTS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toMention(reviews, viewerID, groups, tt.m, tt.parent)
			if ok != (tt.kind != tagNone) {
				t.Fatalf("mention %v, want %v: %+v", ok, tt.kind != tagNone, got)
			}
			if !ok {
				return
			}
			if got.kind != tt.kind || !reflect.DeepEqual(got.prs, tt.prs) || got.threadTS != tt.threadTS ||
				got.ts != tt.m.TS || got.user != tt.m.User || got.channel != reviews {
				t.Errorf("got %+v, want kind %v, prs %v, thread %q", got, tt.kind, tt.prs, tt.threadTS)
			}
		})
	}
}

func TestSimplify(t *testing.T) {
	names := map[string]string{"UALICE": "alice"}
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}
	long := "<@UALICE> " + strings.Repeat("abcdefghij ", 11)
	tests := []struct {
		name, in, want string
	}{
		{name: "plain", in: "please review", want: "please review"},
		{name: "link with label", in: "see <https://github.com/acme/api/pull/1|acme/api#1>", want: "see acme/api#1"},
		{name: "link", in: "see <https://github.com/acme/api/pull/1>", want: "see https://github.com/acme/api/pull/1"},
		{name: "user with label", in: "<@U1|bob> ping", want: "@bob ping"},
		{name: "user", in: "<@UALICE> ping", want: "@alice ping"},
		{name: "unknown user", in: "<@UX> ping", want: "@UX ping"},
		{name: "group with label", in: "can someone from <!subteam^SREV|@reviewers> take this?",
			want: "can someone from @reviewers take this?"},
		{name: "group", in: "<!subteam^SREV> please", want: "@team please"},
		{name: "here", in: "<!here> please", want: "@here please"},
		{name: "channel ref", in: "moved to <#C2|dev-chat>", want: "moved to #dev-chat"},
		{name: "escapes", in: "a &amp;&amp; b &lt;c&gt; &amp;lt;", want: "a && b <c> &lt;"},
		{name: "newlines", in: "first line\n\nsecond\tline ", want: "first line second line"},
		{name: "truncated", in: long, want: "@alice " + strings.Repeat("abcdefghij ", 8) + "abcd…"},
		{name: "truncated at a space", in: strings.Repeat("abcdefghij ", 10),
			want: strings.Repeat("abcdefghij ", 8) + "abcdefghij…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := simplify(tt.in, name)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if n := len([]rune(got)); n > titleMax {
				t.Errorf("%d runes, want at most %d", n, titleMax)
			}
		})
	}
}

func TestPermalink(t *testing.T) {
	tests := []struct {
		name, team, ts, thread, want string
	}{
		{name: "message", team: teamURL, ts: "1760000000.000100",
			want: "https://acme-co.slack.com/archives/C1/p1760000000000100"},
		{name: "team URL without slash", team: "https://acme-co.slack.com", ts: "1760000000.000100",
			want: "https://acme-co.slack.com/archives/C1/p1760000000000100"},
		{name: "thread parent", team: teamURL, ts: "1760000000.000100", thread: "1760000000.000100",
			want: "https://acme-co.slack.com/archives/C1/p1760000000000100"},
		{name: "reply", team: teamURL, ts: "1760000100.000200", thread: "1760000000.000100",
			want: "https://acme-co.slack.com/archives/C1/p1760000100000200?thread_ts=1760000000.000100&cid=C1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := permalink(tt.team, "C1", tt.ts, tt.thread); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTSTime(t *testing.T) {
	tests := []struct {
		ts   string
		want time.Time
		ok   bool
	}{
		{ts: "1760000000.000100", want: time.Unix(1760000000, 100_000), ok: true},
		{ts: "1760000000", want: time.Unix(1760000000, 0), ok: true},
		{ts: "1760000000.5", want: time.Unix(1760000000, 500_000_000), ok: true},
		{ts: ""},
		{ts: "abc.1"},
		{ts: "1760000000.x"},
	}
	for _, tt := range tests {
		got, ok := tsTime(tt.ts)
		if ok != tt.ok || !got.Equal(tt.want) {
			t.Errorf("tsTime(%q) = %v, %v; want %v, %v", tt.ts, got, ok, tt.want, tt.ok)
		}
	}
}
