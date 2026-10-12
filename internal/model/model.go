// Package model holds the domain types shared by all dev-inbox packages.
package model

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type PRKey struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (k PRKey) String() string { return fmt.Sprintf("%s/%s#%d", k.Owner, k.Repo, k.Number) }

type PRState string

const (
	PRStateOpen   PRState = "OPEN"
	PRStateClosed PRState = "CLOSED"
	PRStateMerged PRState = "MERGED"
)

// ChecksState mirrors GitHub's StatusCheckRollup state of the head commit, collapsed to four values.
type ChecksState string

const (
	ChecksNone    ChecksState = ""        // head commit has no checks
	ChecksPending ChecksState = "PENDING" // PENDING or EXPECTED
	ChecksSuccess ChecksState = "SUCCESS"
	ChecksFailure ChecksState = "FAILURE" // FAILURE or ERROR
)

// PRStatus is the cheap-to-fetch part of a PR. Reconciliation compares it against the stored
// snapshot to decide whether a full fetch is needed.
type PRStatus struct {
	State     PRState     `json:"state"`
	IsDraft   bool        `json:"isDraft"`
	HeadSHA   string      `json:"headSha"`
	UpdatedAt time.Time   `json:"updatedAt"`
	Checks    ChecksState `json:"checks"`
	// ChecksRunning counts check runs and status contexts of the head commit that are queued, in
	// progress, pending, waiting or expected.
	ChecksRunning int `json:"checksRunning"`
	// ChecksFailing counts those that concluded FAILURE, ERROR, TIMED_OUT, CANCELLED, STARTUP_FAILURE
	// or ACTION_REQUIRED.
	ChecksFailing    int    `json:"checksFailing"`
	Mergeable        string `json:"mergeable"`        // MERGEABLE, CONFLICTING, UNKNOWN
	MergeStateStatus string `json:"mergeStateStatus"` // CLEAN, BLOCKED, BEHIND, DIRTY, UNSTABLE, HAS_HOOKS, DRAFT, UNKNOWN
	ReviewDecision   string `json:"reviewDecision"`   // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED, "" without required reviews
	InMergeQueue     bool   `json:"inMergeQueue"`
}

type PR struct {
	Key PRKey `json:"key"`
	PRStatus
	NodeID         string          `json:"nodeId"`
	Title          string          `json:"title"`
	URL            string          `json:"url"`
	Author         string          `json:"author"`
	BaseRef        string          `json:"baseRef"`
	HeadRef        string          `json:"headRef"`
	Labels         []string        `json:"labels,omitempty"`
	Assignees      []string        `json:"assignees,omitempty"`
	Additions      int             `json:"additions"`
	Deletions      int             `json:"deletions"`
	ChangedFiles   int             `json:"changedFiles"`
	CreatedAt      time.Time       `json:"createdAt"`
	LastPushAt     time.Time       `json:"lastPushAt"` // best effort: committed date of the head commit
	ReviewRequests []ReviewRequest `json:"reviewRequests,omitempty"`
	Reviews        []Review        `json:"reviews,omitempty"` // submitted reviews, oldest first
	CheckRuns      []CheckRun      `json:"checkRuns,omitempty"`
	Comments       []Comment       `json:"comments,omitempty"` // oldest first; only the most recent ones are fetched
	FetchedAt      time.Time       `json:"fetchedAt"`
}

// ReviewRequest is a pending review request for either a user (Login) or a team (Team as "org/slug").
type ReviewRequest struct {
	Login string `json:"login,omitempty"`
	Team  string `json:"team,omitempty"`
}

type Review struct {
	ID          string    `json:"id"`
	Author      string    `json:"author"`
	State       string    `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED
	SubmittedAt time.Time `json:"submittedAt"`
}

type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // QUEUED, IN_PROGRESS, COMPLETED, ...; status contexts map their state here
	Conclusion string `json:"conclusion"` // SUCCESS, FAILURE, NEUTRAL, ...; empty while running
	URL        string `json:"url"`
}

type CommentKind string

const (
	CommentIssue  CommentKind = "issue"  // conversation comment
	CommentReview CommentKind = "review" // non-empty review summary body
	CommentInline CommentKind = "inline" // review comment on a diff line
)

type Comment struct {
	ID        string      `json:"id"` // GraphQL node ID
	Kind      CommentKind `json:"kind"`
	Author    string      `json:"author"`
	Body      string      `json:"body"`
	URL       string      `json:"url"`
	Path      string      `json:"path,omitempty"`
	Line      int         `json:"line,omitempty"`
	ReplyTo   string      `json:"replyTo,omitempty"` // node ID of the inline comment this one replies to
	CreatedAt time.Time   `json:"createdAt"`
}

type Viewer struct {
	Login string   `json:"login"`
	Teams []string `json:"teams,omitempty"` // "org/slug"
}

type Section string

const (
	SectionNone             Section = "" // closed or merged
	SectionReviewRequested  Section = "review_requested"
	SectionNeedsAction      Section = "needs_action"
	SectionReadyToMerge     Section = "ready_to_merge"
	SectionDrafts           Section = "drafts"
	SectionWaitingForReview Section = "waiting_for_review"
	SectionInvolved         Section = "involved"
)

// Sections lists the inbox sections in display order.
var Sections = []Section{
	SectionReviewRequested,
	SectionNeedsAction,
	SectionReadyToMerge,
	SectionDrafts,
	SectionWaitingForReview,
	SectionInvolved,
}

type Classification struct {
	Section Section `json:"section"`
	// Direct is true when review is requested from the viewer personally rather than via a team.
	Direct bool `json:"direct,omitempty"`
	// Reasons explains needs_action, e.g. "checks failing", "changes requested", "merge conflict", "new comments".
	Reasons []string `json:"reasons,omitempty"`
	// Score is the priority inside a section; higher sorts first. See derive.Score.
	Score int `json:"score"`
}

// Annotation is enrichment data attached by a workflow to a PR or, with CommentID set, to one of its comments.
// A JSON object value with a "badge" string (and optional "level": info|warn|crit) is rendered as a badge by the TUI.
type Annotation struct {
	PR        PRKey           `json:"pr"`
	CommentID string          `json:"commentId,omitempty"`
	Key       string          `json:"key"` // namespaced, e.g. "linear.urgency"
	Value     json.RawMessage `json:"value"`
	Source    string          `json:"source,omitempty"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

const (
	EventDiscovered           = "pr.discovered"
	EventUpdated              = "pr.updated"
	EventReviewRequested      = "pr.review_requested"
	EventReviewRequestRemoved = "pr.review_request_removed"
	EventCommitsPushed        = "pr.commits_pushed"
	EventReviewSubmitted      = "pr.review_submitted"
	EventCommentAdded         = "pr.comment_added"
	EventChecksFailed         = "pr.checks_failed"
	EventChecksPassed         = "pr.checks_passed"
	EventDraftChanged         = "pr.draft_changed"
	EventSectionChanged       = "pr.section_changed"
	EventMerged               = "pr.merged"
	EventClosed               = "pr.closed"
	EventReopened             = "pr.reopened"
	EventAnnotated            = "pr.annotated"
)

type Event struct {
	ID   string          `json:"id"`
	Type string          `json:"type"` // one of the Event* constants
	PR   PRKey           `json:"pr"`
	Time time.Time       `json:"time"`
	Data json.RawMessage `json:"data"` // encoded EventData
}

// EventData is the payload of every event.
type EventData struct {
	PR PR `json:"pr"` // snapshot after the change, without comments
	Classification
	Change any `json:"change,omitempty"` // one of the Change* types, depending on the event type
}

// ChangeReviewRequest is the change of pr.review_requested and pr.review_request_removed.
type ChangeReviewRequest struct {
	ReviewRequest
	Direct bool `json:"direct"`
}

// ChangeCommits is the change of pr.commits_pushed.
type ChangeCommits struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// ChangeChecks is the change of pr.checks_failed and pr.checks_passed.
type ChangeChecks struct {
	Before ChecksState `json:"before"`
	After  ChecksState `json:"after"`
}

// ChangeDraft is the change of pr.draft_changed.
type ChangeDraft struct {
	IsDraft bool `json:"isDraft"`
}

// ChangeSection is the change of pr.section_changed.
type ChangeSection struct {
	From Section `json:"from"`
	To   Section `json:"to"`
}

// pr.review_submitted carries a Review, pr.comment_added a Comment, pr.annotated an Annotation.

// NewEvent creates an event with a time-ordered unique ID and data encoded as JSON.
func NewEvent(typ string, pr PRKey, data EventData) (Event, error) {
	data.PR.Comments = nil
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("encode %s event data: %w", typ, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id.String(), Type: typ, PR: pr, Time: time.Now().UTC(), Data: raw}, nil
}
