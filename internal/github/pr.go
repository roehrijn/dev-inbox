package github

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/roehrijn/dev-inbox/internal/model"
)

const statusFragment = `fragment Status on PullRequest {
  state
  isDraft
  headRefOid
  updatedAt
  mergeable
  mergeStateStatus
  reviewDecision
  isInMergeQueue
  rollupCommit: commits(last: 1) {
    nodes {
      commit {
        statusCheckRollup {
          state
          contexts(first: 100) {
            checkRunCountsByState { state count }
            statusContextCountsByState { state count }
            nodes {
              __typename
              ... on CheckRun { databaseId name status conclusion detailsUrl checkSuite { workflowRun { workflow { name } } } }
              ... on StatusContext { context state targetUrl }
            }
          }
        }
      }
    }
  }
}
`

const prQuery = `query PullRequest($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    name
    owner { login }
    pullRequest(number: $number) {
      ...Status
      id
      title
      url
      author { __typename login }
      baseRefName
      headRefName
      labels(first: 100) { nodes { name } }
      assignees(first: 100) { nodes { login } }
      additions
      deletions
      changedFiles
      createdAt
      headCommit: commits(last: 1) {
        nodes {
          commit { committedDate }
        }
      }
      reviewRequests(first: 100) {
        nodes {
          requestedReviewer {
            __typename
            ... on User { login }
            ... on Bot { login }
            ... on Mannequin { login }
            ... on Team { combinedSlug }
          }
        }
      }
      reviews(last: 100) {
        nodes { id author { __typename login } state body url submittedAt commit { oid } reactionGroups { content viewerHasReacted } }
      }
      comments(last: 100) {
        totalCount
        nodes { id author { __typename login } body url createdAt reactionGroups { content viewerHasReacted } }
      }
      reviewThreads(first: 50) { ...ReviewThreadsPage }
    }
  }
}
` + statusFragment + threadsFragment

// threadsFragment is shared by the first page of review threads in prQuery and the follow-up pages
// of threadsQuery. Comments per thread are capped at 50; FetchPR logs threads that have more. A
// comment's SHA is originalCommit: commit follows the PR head for comments that are not outdated.
const threadsFragment = `fragment ReviewThreadsPage on PullRequestReviewThreadConnection {
  pageInfo { hasNextPage endCursor }
  nodes {
    id
    isResolved
    resolvedBy { __typename login }
    comments(first: 50) {
      totalCount
      nodes { id author { __typename login } body url createdAt path line originalLine state replyTo { id } originalCommit { oid } reactionGroups { content viewerHasReacted } }
    }
  }
}
`

const threadsQuery = `query ReviewThreads($owner: String!, $repo: String!, $number: Int!, $after: String!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 50, after: $after) { ...ReviewThreadsPage }
    }
  }
}
` + threadsFragment

type gqlActor struct {
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

// login renders an actor like the REST API does: bots get the "[bot]" suffix that GraphQL omits,
// and deleted accounts (null author) become "ghost".
func login(a *gqlActor) string {
	if a == nil {
		return "ghost"
	}
	if a.Typename == "Bot" && !strings.HasSuffix(a.Login, "[bot]") {
		return a.Login + "[bot]"
	}
	return a.Login
}

// gqlReactions is the reactionGroups of a comment: one entry per reaction content, whether or not
// anyone used it.
type gqlReactions []struct {
	Content          string `json:"content"`
	ViewerHasReacted bool   `json:"viewerHasReacted"`
}

// viewer returns the contents the viewer reacted with, in GraphQL order.
func (r gqlReactions) viewer() []string {
	var out []string
	for _, g := range r {
		if g.ViewerHasReacted {
			out = append(out, g.Content)
		}
	}
	return out
}

// gqlCommit is a nullable commit reference.
type gqlCommit struct {
	OID string `json:"oid"`
}

func (c *gqlCommit) sha() string {
	if c == nil {
		return ""
	}
	return c.OID
}

type gqlStatus struct {
	State            string    `json:"state"`
	IsDraft          bool      `json:"isDraft"`
	HeadRefOid       string    `json:"headRefOid"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Mergeable        string    `json:"mergeable"`
	MergeStateStatus string    `json:"mergeStateStatus"`
	ReviewDecision   string    `json:"reviewDecision"`
	IsInMergeQueue   bool      `json:"isInMergeQueue"`
	RollupCommit     struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *gqlRollup `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"rollupCommit"`
}

// gqlRollup is the status check rollup of a commit. Its counts cover every check, unlike the
// capped contexts nodes.
type gqlRollup struct {
	State    string `json:"state"`
	Contexts struct {
		CheckRunCountsByState      []gqlStateCount   `json:"checkRunCountsByState"`
		StatusContextCountsByState []gqlStateCount   `json:"statusContextCountsByState"`
		Nodes                      []gqlCheckContext `json:"nodes"`
	} `json:"contexts"`
}

type gqlStateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// checkRunBucket classifies a check run state. States not listed, including ones GitHub adds
// later, are neither running nor failing.
func checkRunBucket(state string) (running, failing bool) {
	switch state {
	case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED":
		return true, false
	case "FAILURE", "TIMED_OUT", "CANCELLED", "STARTUP_FAILURE", "ACTION_REQUIRED":
		return false, true
	}
	return false, false
}

// counts buckets the checks by state, leaving out superseded check runs, and reports whether there
// were any.
func (r gqlRollup) counts() (running, failing int, dropped bool) {
	add := func(isRunning, isFailing bool, n int) {
		if isRunning {
			running += n
		}
		if isFailing {
			failing += n
		}
	}
	for _, c := range r.Contexts.CheckRunCountsByState {
		isRunning, isFailing := checkRunBucket(c.State)
		add(isRunning, isFailing, c.Count)
	}
	for _, c := range r.Contexts.StatusContextCountsByState {
		add(c.State == "PENDING" || c.State == "EXPECTED", c.State == "FAILURE" || c.State == "ERROR", c.Count)
	}
	_, old := latestContexts(r.Contexts.Nodes)
	for _, cc := range old {
		isRunning, isFailing := checkRunBucket(cmp.Or(cc.Conclusion, cc.Status))
		add(isRunning, isFailing, -1)
	}
	return running, failing, len(old) > 0
}

// latestContexts splits check contexts into the ones GitHub shows and the check runs a newer run of
// the same check (by workflow and name) superseded, such as a run cancelled by its successor. The
// rollup state and counts still include superseded runs.
func latestContexts(nodes []gqlCheckContext) (latest, superseded []gqlCheckContext) {
	newest := map[string]int64{}
	for _, cc := range nodes {
		if k, ok := cc.checkKey(); ok {
			newest[k] = max(newest[k], cc.DatabaseID)
		}
	}
	for _, cc := range nodes {
		if k, ok := cc.checkKey(); ok && cc.DatabaseID < newest[k] {
			superseded = append(superseded, cc)
			continue
		}
		latest = append(latest, cc)
	}
	return latest, superseded
}

func (s gqlStatus) toModel() model.PRStatus {
	st := model.PRStatus{
		State:            model.PRState(s.State),
		IsDraft:          s.IsDraft,
		HeadSHA:          s.HeadRefOid,
		UpdatedAt:        s.UpdatedAt,
		Mergeable:        s.Mergeable,
		MergeStateStatus: s.MergeStateStatus,
		ReviewDecision:   s.ReviewDecision,
		InMergeQueue:     s.IsInMergeQueue,
	}
	if n := s.RollupCommit.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		r := n[0].Commit.StatusCheckRollup
		var dropped bool
		st.ChecksRunning, st.ChecksFailing, dropped = r.counts()
		st.Checks = collapseChecks(r.State)
		if dropped {
			st.Checks = model.ChecksSuccess
			switch {
			case st.ChecksFailing > 0:
				st.Checks = model.ChecksFailure
			case st.ChecksRunning > 0:
				st.Checks = model.ChecksPending
			}
		}
	}
	return st
}

func collapseChecks(state string) model.ChecksState {
	switch state {
	case "":
		return model.ChecksNone
	case "SUCCESS":
		return model.ChecksSuccess
	case "FAILURE", "ERROR":
		return model.ChecksFailure
	default:
		return model.ChecksPending
	}
}

type gqlPR struct {
	gqlStatus
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Author       *gqlActor `json:"author"`
	BaseRefName  string    `json:"baseRefName"`
	HeadRefName  string    `json:"headRefName"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changedFiles"`
	CreatedAt    time.Time `json:"createdAt"`
	Labels       struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
	HeadCommit struct {
		Nodes []struct {
			Commit struct {
				CommittedDate     time.Time `json:"committedDate"`
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []gqlCheckContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"headCommit"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Typename     string `json:"__typename"`
				Login        string `json:"login"`
				CombinedSlug string `json:"combinedSlug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	Reviews struct {
		Nodes []struct {
			ID          string       `json:"id"`
			Author      *gqlActor    `json:"author"`
			State       string       `json:"state"`
			Body        string       `json:"body"`
			URL         string       `json:"url"`
			SubmittedAt time.Time    `json:"submittedAt"`
			Commit      *gqlCommit   `json:"commit"`
			Reactions   gqlReactions `json:"reactionGroups"`
		} `json:"nodes"`
	} `json:"reviews"`
	Comments struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			ID        string       `json:"id"`
			Author    *gqlActor    `json:"author"`
			Body      string       `json:"body"`
			URL       string       `json:"url"`
			CreatedAt time.Time    `json:"createdAt"`
			Reactions gqlReactions `json:"reactionGroups"`
		} `json:"nodes"`
	} `json:"comments"`
	ReviewThreads gqlThreads `json:"reviewThreads"`
}

type gqlThreads struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []gqlThread `json:"nodes"`
}

type gqlThread struct {
	ID         string    `json:"id"`
	IsResolved bool      `json:"isResolved"`
	ResolvedBy *gqlActor `json:"resolvedBy"`
	Comments   struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			ID           string    `json:"id"`
			Author       *gqlActor `json:"author"`
			Body         string    `json:"body"`
			URL          string    `json:"url"`
			CreatedAt    time.Time `json:"createdAt"`
			Path         string    `json:"path"`
			Line         *int      `json:"line"`
			OriginalLine *int      `json:"originalLine"`
			State        string    `json:"state"`
			ReplyTo      *struct {
				ID string `json:"id"`
			} `json:"replyTo"`
			OriginalCommit *gqlCommit   `json:"originalCommit"`
			Reactions      gqlReactions `json:"reactionGroups"`
		} `json:"nodes"`
	} `json:"comments"`
}

// gqlCheckContext is either a CheckRun (name, status, conclusion, detailsUrl) or a StatusContext
// (context, state, targetUrl).
type gqlCheckContext struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	Context    string `json:"context"`
	State      string `json:"state"`
	TargetURL  string `json:"targetUrl"`
	DatabaseID int64  `json:"databaseId"`
	CheckSuite *struct {
		WorkflowRun *struct {
			Workflow struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
}

// checkKey identifies a check run across reruns; status contexts are already unique per context.
func (cc gqlCheckContext) checkKey() (string, bool) {
	if cc.Typename != "CheckRun" {
		return "", false
	}
	var workflow string
	if cc.CheckSuite != nil && cc.CheckSuite.WorkflowRun != nil {
		workflow = cc.CheckSuite.WorkflowRun.Workflow.Name
	}
	return workflow + "\x00" + cc.Name, true
}

func (cc gqlCheckContext) toModel() model.CheckRun {
	if cc.Typename != "StatusContext" {
		return model.CheckRun{Name: cc.Name, Status: cc.Status, Conclusion: cc.Conclusion, URL: cc.DetailsURL}
	}
	cr := model.CheckRun{Name: cc.Context, Status: cc.State, URL: cc.TargetURL}
	if cc.State != "PENDING" && cc.State != "EXPECTED" {
		cr.Conclusion = cc.State
	}
	return cr
}

func (p *gqlPR) toModel(key model.PRKey, fetchedAt time.Time) model.PR {
	pr := model.PR{
		Key:          key,
		PRStatus:     p.gqlStatus.toModel(),
		NodeID:       p.ID,
		Title:        p.Title,
		URL:          p.URL,
		Author:       login(p.Author),
		BaseRef:      p.BaseRefName,
		HeadRef:      p.HeadRefName,
		Additions:    p.Additions,
		Deletions:    p.Deletions,
		ChangedFiles: p.ChangedFiles,
		CreatedAt:    p.CreatedAt,
		FetchedAt:    fetchedAt,
	}
	for _, l := range p.Labels.Nodes {
		pr.Labels = append(pr.Labels, l.Name)
	}
	for _, a := range p.Assignees.Nodes {
		pr.Assignees = append(pr.Assignees, a.Login)
	}
	if n := p.HeadCommit.Nodes; len(n) > 0 {
		pr.LastPushAt = n[0].Commit.CommittedDate
	}
	if n := p.RollupCommit.Nodes; len(n) > 0 && n[0].Commit.StatusCheckRollup != nil {
		latest, _ := latestContexts(n[0].Commit.StatusCheckRollup.Contexts.Nodes)
		for _, cc := range latest {
			pr.CheckRuns = append(pr.CheckRuns, cc.toModel())
		}
	}
	for _, n := range p.ReviewRequests.Nodes {
		r := n.RequestedReviewer
		switch {
		case r == nil:
		case r.Typename == "Team":
			pr.ReviewRequests = append(pr.ReviewRequests, model.ReviewRequest{Team: r.CombinedSlug})
		default:
			pr.ReviewRequests = append(pr.ReviewRequests, model.ReviewRequest{Login: login(&gqlActor{Typename: r.Typename, Login: r.Login})})
		}
	}
	for _, r := range p.Reviews.Nodes {
		if r.State == "PENDING" || r.SubmittedAt.IsZero() {
			continue
		}
		author := login(r.Author)
		pr.Reviews = append(pr.Reviews, model.Review{
			ID: r.ID, Author: author, State: r.State, SubmittedAt: r.SubmittedAt, CommitSHA: r.Commit.sha(),
		})
		if strings.TrimSpace(r.Body) != "" {
			pr.Comments = append(pr.Comments, model.Comment{
				ID: r.ID, Kind: model.CommentReview, Author: author, Body: r.Body, URL: r.URL, CreatedAt: r.SubmittedAt,
				ViewerReactions: r.Reactions.viewer(),
			})
		}
	}
	slices.SortStableFunc(pr.Reviews, func(a, b model.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	for _, c := range p.Comments.Nodes {
		pr.Comments = append(pr.Comments, model.Comment{
			ID: c.ID, Kind: model.CommentIssue, Author: login(c.Author), Body: c.Body, URL: c.URL, CreatedAt: c.CreatedAt,
			ViewerReactions: c.Reactions.viewer(),
		})
	}
	for _, t := range p.ReviewThreads.Nodes {
		var inline []model.Comment
		for _, c := range t.Comments.Nodes {
			if c.State == "PENDING" {
				continue
			}
			cm := model.Comment{
				ID: c.ID, Kind: model.CommentInline, Author: login(c.Author), Body: c.Body, URL: c.URL,
				Path: c.Path, ThreadID: t.ID, CommitSHA: c.OriginalCommit.sha(), CreatedAt: c.CreatedAt,
				ViewerReactions: c.Reactions.viewer(),
			}
			switch {
			case c.Line != nil:
				cm.Line = *c.Line
			case c.OriginalLine != nil:
				cm.Line = *c.OriginalLine
			}
			if c.ReplyTo != nil {
				cm.ReplyTo = c.ReplyTo.ID
			}
			inline = append(inline, cm)
		}
		if len(inline) == 0 && len(t.Comments.Nodes) > 0 {
			continue // only the viewer's unsent draft comments
		}
		pr.Comments = append(pr.Comments, inline...)
		rt := model.ReviewThread{ID: t.ID, IsResolved: t.IsResolved}
		if t.ResolvedBy != nil {
			rt.ResolvedBy = login(t.ResolvedBy)
		}
		pr.ReviewThreads = append(pr.ReviewThreads, rt)
	}
	slices.SortStableFunc(pr.Comments, func(a, b model.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return pr
}
