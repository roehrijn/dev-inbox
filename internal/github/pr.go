package github

import (
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
          contexts { checkRunCountsByState { state count } statusContextCountsByState { state count } }
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
          commit {
            committedDate
            statusCheckRollup {
              contexts(first: 100) {
                nodes {
                  __typename
                  ... on CheckRun { name status conclusion detailsUrl }
                  ... on StatusContext { context state targetUrl }
                }
              }
            }
          }
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
        nodes { id author { __typename login } state body url submittedAt }
      }
      comments(last: 100) {
        nodes { id author { __typename login } body url createdAt }
      }
      reviewThreads(last: 50) {
        nodes {
          comments(first: 50) {
            nodes { id author { __typename login } body url createdAt path line originalLine state replyTo { id } }
          }
        }
      }
    }
  }
}
` + statusFragment

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
// capped contexts nodes of the full PR query.
type gqlRollup struct {
	State    string `json:"state"`
	Contexts struct {
		CheckRunCountsByState      []gqlStateCount `json:"checkRunCountsByState"`
		StatusContextCountsByState []gqlStateCount `json:"statusContextCountsByState"`
	} `json:"contexts"`
}

type gqlStateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

// counts buckets the checks by state. States not listed, including ones GitHub adds later, are
// neither running nor failing.
func (r gqlRollup) counts() (running, failing int) {
	for _, c := range r.Contexts.CheckRunCountsByState {
		switch c.State {
		case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED":
			running += c.Count
		case "FAILURE", "TIMED_OUT", "CANCELLED", "STARTUP_FAILURE", "ACTION_REQUIRED":
			failing += c.Count
		}
	}
	for _, c := range r.Contexts.StatusContextCountsByState {
		switch c.State {
		case "PENDING", "EXPECTED":
			running += c.Count
		case "FAILURE", "ERROR":
			failing += c.Count
		}
	}
	return running, failing
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
		st.Checks = collapseChecks(r.State)
		st.ChecksRunning, st.ChecksFailing = r.counts()
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
			ID          string    `json:"id"`
			Author      *gqlActor `json:"author"`
			State       string    `json:"state"`
			Body        string    `json:"body"`
			URL         string    `json:"url"`
			SubmittedAt time.Time `json:"submittedAt"`
		} `json:"nodes"`
	} `json:"reviews"`
	Comments struct {
		Nodes []struct {
			ID        string    `json:"id"`
			Author    *gqlActor `json:"author"`
			Body      string    `json:"body"`
			URL       string    `json:"url"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
	ReviewThreads struct {
		Nodes []struct {
			Comments struct {
				Nodes []struct {
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
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
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
		if r := n[0].Commit.StatusCheckRollup; r != nil {
			for _, cc := range r.Contexts.Nodes {
				pr.CheckRuns = append(pr.CheckRuns, cc.toModel())
			}
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
		pr.Reviews = append(pr.Reviews, model.Review{ID: r.ID, Author: author, State: r.State, SubmittedAt: r.SubmittedAt})
		if strings.TrimSpace(r.Body) != "" {
			pr.Comments = append(pr.Comments, model.Comment{
				ID: r.ID, Kind: model.CommentReview, Author: author, Body: r.Body, URL: r.URL, CreatedAt: r.SubmittedAt,
			})
		}
	}
	slices.SortStableFunc(pr.Reviews, func(a, b model.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	for _, c := range p.Comments.Nodes {
		pr.Comments = append(pr.Comments, model.Comment{
			ID: c.ID, Kind: model.CommentIssue, Author: login(c.Author), Body: c.Body, URL: c.URL, CreatedAt: c.CreatedAt,
		})
	}
	for _, t := range p.ReviewThreads.Nodes {
		for _, c := range t.Comments.Nodes {
			if c.State == "PENDING" {
				continue
			}
			cm := model.Comment{
				ID: c.ID, Kind: model.CommentInline, Author: login(c.Author), Body: c.Body, URL: c.URL,
				Path: c.Path, CreatedAt: c.CreatedAt,
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
			pr.Comments = append(pr.Comments, cm)
		}
	}
	slices.SortStableFunc(pr.Comments, func(a, b model.Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return pr
}
