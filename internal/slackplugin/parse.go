package slackplugin

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
	"github.com/roehrijn/dev-inbox/internal/slack"
)

type tagKind int

const (
	tagNone tagKind = iota
	tagGroup
	tagDirect
)

// mention is a message that tags the viewer and links at least one PR.
type mention struct {
	channel slack.Channel
	ts      string
	// threadTS is the TS of the thread parent for a reply, empty otherwise.
	threadTS string
	user     string
	text     string
	kind     tagKind
	prs      []model.PRKey
	time     time.Time
}

// tags reports how m tags the viewer: directly, through one of groups, or not at all. Messages by the
// viewer and by bots never tag; @here and @channel do not count.
func tags(m slack.Message, viewer string, groups []string) tagKind {
	if m.User == viewer || m.BotID != "" || m.Subtype == "bot_message" {
		return tagNone
	}
	if containsRef(m.Text, "<@"+viewer) {
		return tagDirect
	}
	for _, g := range groups {
		if containsRef(m.Text, "<!subteam^"+g) {
			return tagGroup
		}
	}
	return tagNone
}

// containsRef reports whether text contains ref closed by ">" or followed by a "|label".
func containsRef(text, ref string) bool {
	return strings.Contains(text, ref+">") || strings.Contains(text, ref+"|")
}

var prLinkRE = regexp.MustCompile(`https?://(?i:github\.com)/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/pull/([0-9]+)`)

// prLinks returns the PRs linked in text, deduplicated, in order of appearance. A link ends after the
// number, before "|", ">", "/", "#", "?", whitespace or the end of the text.
func prLinks(text string) []model.PRKey {
	var out []model.PRKey
	for _, loc := range prLinkRE.FindAllStringSubmatchIndex(text, -1) {
		if end := loc[1]; end < len(text) && !strings.ContainsRune("|>/#? \t\r\n", rune(text[end])) {
			continue
		}
		n, err := strconv.Atoi(text[loc[6]:loc[7]])
		if err != nil || n <= 0 {
			continue
		}
		k := model.PRKey{Owner: text[loc[2]:loc[3]], Repo: text[loc[4]:loc[5]], Number: n}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// isReply reports whether m is a thread reply rather than a top-level message or thread parent.
func isReply(m slack.Message) bool { return m.ThreadTS != "" && m.ThreadTS != m.TS }

// toMention returns the mention of m in ch, if m tags the viewer and links a PR. parent is the thread
// parent of a reply, nil otherwise; a tagging reply without PR links inherits the parent's.
func toMention(ch slack.Channel, viewer string, groups []string, m slack.Message, parent *slack.Message) (mention, bool) {
	kind := tags(m, viewer, groups)
	if kind == tagNone {
		return mention{}, false
	}
	t, ok := tsTime(m.TS)
	if !ok {
		return mention{}, false
	}
	prs := prLinks(m.Text)
	var threadTS string
	if isReply(m) {
		threadTS = m.ThreadTS
	}
	if parent != nil {
		threadTS = parent.TS
		if len(prs) == 0 {
			prs = prLinks(parent.Text)
		}
	}
	if len(prs) == 0 {
		return mention{}, false
	}
	return mention{channel: ch, ts: m.TS, threadTS: threadTS, user: m.User, text: m.Text, kind: kind, prs: prs, time: t}, true
}

// tsTime parses a Slack message timestamp such as "1760000000.000100".
func tsTime(ts string) (time.Time, bool) {
	secText, fracText, _ := strings.Cut(ts, ".")
	sec, err := strconv.ParseInt(secText, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	var nsec int64
	if fracText != "" {
		if len(fracText) > 9 {
			fracText = fracText[:9]
		}
		f, err := strconv.ParseInt(fracText, 10, 64)
		if err != nil || f < 0 {
			return time.Time{}, false
		}
		for i := len(fracText); i < 9; i++ {
			f *= 10
		}
		nsec = f
	}
	return time.Unix(sec, nsec), true
}

// permalink returns the URL of message ts in channel; threadTS is the parent of a reply, empty otherwise.
func permalink(teamURL, channel, ts, threadTS string) string {
	u := strings.TrimSuffix(teamURL, "/") + "/archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)
	if threadTS != "" && threadTS != ts {
		u += "?thread_ts=" + threadTS + "&cid=" + channel
	}
	return u
}

var (
	markupRE  = regexp.MustCompile(`<([^<>]*)>`)
	unescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&")
)

// simplify turns Slack message markup into plain text of at most titleMax runes. name resolves the user
// IDs of mentions without a label.
func simplify(text string, name func(userID string) string) string {
	text = markupRE.ReplaceAllStringFunc(text, func(s string) string {
		ref, label, hasLabel := strings.Cut(s[1:len(s)-1], "|")
		switch {
		case strings.HasPrefix(ref, "@"):
			if hasLabel {
				return "@" + strings.TrimPrefix(label, "@")
			}
			return "@" + name(ref[1:])
		case strings.HasPrefix(ref, "#"):
			if hasLabel {
				return "#" + strings.TrimPrefix(label, "#")
			}
			return ref
		case strings.HasPrefix(ref, "!subteam^"):
			if hasLabel {
				return "@" + strings.TrimPrefix(label, "@")
			}
			return "@team"
		case strings.HasPrefix(ref, "!"):
			if hasLabel {
				return label
			}
			return "@" + ref[1:]
		case hasLabel:
			return label
		default:
			return ref
		}
	})
	text = strings.Join(strings.Fields(unescaper.Replace(text)), " ")
	return truncate(text, titleMax)
}

func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}
