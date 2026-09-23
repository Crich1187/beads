package tracker

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

// protectedAssignees matches local assignees that pull must leave alone,
// typically agent claims (see SyncOptions.ProtectedAssigneePatterns).
type protectedAssignees []*regexp.Regexp

// compileProtectedAssignees compiles each pattern as a case-insensitive
// whole-string match. Blank patterns are ignored.
func compileProtectedAssignees(patterns []string) (protectedAssignees, error) {
	var compiled protectedAssignees
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		re, err := regexp.Compile(`(?i)^(?:` + pattern + `)$`)
		if err != nil {
			return nil, fmt.Errorf("invalid protected assignee pattern %q: %w", pattern, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

func (p protectedAssignees) matches(assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	for _, re := range p {
		if re.MatchString(assignee) {
			return true
		}
	}
	return false
}

// pullAssignee returns the assignee a pull should write to an existing issue,
// and false when the local assignee must stay untouched: the remote assignee
// is empty (pull never clears a local assignee), or the local assignee is a
// protected claim.
func pullAssignee(local, remote *types.Issue, protected protectedAssignees) (string, bool) {
	if strings.TrimSpace(remote.Assignee) == "" {
		return "", false
	}
	if protected.matches(local.Assignee) {
		return "", false
	}
	return remote.Assignee, true
}

// pullAssigneeChanges reports whether a pull would change the local assignee.
func pullAssigneeChanges(local, remote *types.Issue, protected protectedAssignees) bool {
	assignee, ok := pullAssignee(local, remote, protected)
	return ok && strings.TrimSpace(assignee) != strings.TrimSpace(local.Assignee)
}
