package github

import (
	"errors"
	"fmt"
	"time"
)

// RateLimitError reports an exhausted primary or secondary rate limit; retry after Reset.
type RateLimitError struct {
	Reset time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github rate limit exceeded, resets at %s", e.Reset.Format(time.RFC3339))
}

// ErrNotFound is returned by FetchPR and TeamMembers when the repository, pull request or team does
// not exist or is not accessible (GraphQL NOT_FOUND or FORBIDDEN). Retrying does not help; other
// errors may be transient.
var ErrNotFound = errors.New("not found")
