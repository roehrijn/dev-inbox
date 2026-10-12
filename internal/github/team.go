package github

import (
	"context"
	"fmt"
	"strings"
)

const teamMembersQuery = `query TeamMembers($org: String!, $slug: String!, $cursor: String) {
  organization(login: $org) {
    team(slug: $slug) {
      members(first: 100, after: $cursor, membership: ALL) {
        nodes { login }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

// TeamMembers returns the logins of all members of team ("org/slug"), including members of child
// teams. An unknown or inaccessible team yields ErrNotFound.
func (c *Client) TeamMembers(ctx context.Context, team string) ([]string, error) {
	org, slug, ok := strings.Cut(team, "/")
	if !ok || org == "" || slug == "" || strings.Contains(slug, "/") {
		return nil, fmt.Errorf("team %q: want org/slug", team)
	}

	var logins []string
	seen := map[string]bool{}
	vars := map[string]any{"org": org, "slug": slug}
	for {
		var data struct {
			Organization *struct {
				Team *struct {
					Members struct {
						Nodes []*struct {
							Login string `json:"login"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"members"`
				} `json:"team"`
			} `json:"organization"`
		}
		errs, err := c.graphQL(ctx, teamMembersQuery, vars, &data)
		if err := queryError(errs, err); err != nil {
			return nil, fmt.Errorf("fetch members of %s: %w", team, err)
		}
		if data.Organization == nil || data.Organization.Team == nil {
			return nil, fmt.Errorf("fetch members of %s: %w", team, ErrNotFound)
		}

		members := data.Organization.Team.Members
		for _, n := range members.Nodes {
			if n != nil && n.Login != "" && !seen[n.Login] {
				seen[n.Login] = true
				logins = append(logins, n.Login)
			}
		}
		if !members.PageInfo.HasNextPage {
			return logins, nil
		}
		if members.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("fetch members of %s: next page without cursor", team)
		}
		vars["cursor"] = members.PageInfo.EndCursor
	}
}
