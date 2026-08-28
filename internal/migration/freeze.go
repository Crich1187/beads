// Package migration provides read-side access to the MIGRATION-FREEZE
// write-freeze marker. The marker is a plain file that an operator — or an
// external orchestration tool driving a migration — creates to stop writes
// against a workspace, and removes to resume them. bd only ever reads it,
// before a write command runs, so a human typing 'bd create'/'bd update'
// mid-migration cannot slip a write past whatever quiesced the workspace
// (dc-6jaq).
package migration

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the name of the freeze marker file. bd looks for it in the
// working directory and every ancestor directory up to the filesystem root,
// so a marker placed at the top of a multi-repo tree freezes every workspace
// beneath it.
const FileName = "MIGRATION-FREEZE"

// EnvFreezeFile names an explicit freeze-marker path. When set and non-empty
// it is authoritative: bd checks that path only and skips the ancestor walk,
// so it doubles as the hook for markers that live outside the working tree
// and as an opt-out (point it at a path that does not exist).
const EnvFreezeFile = "BD_MIGRATION_FREEZE_FILE"

// Info is the parsed contents of a freeze marker.
type Info struct {
	Operator  string    // who initiated the freeze (a username, a tool name)
	Reason    string    // human-readable migration reason
	Timestamp time.Time // when the freeze was set
}

// Find returns the path of the active freeze marker, or "" when writes are
// not frozen. With EnvFreezeFile set, only that path is consulted; otherwise
// the working directory and each of its ancestors are checked for FileName.
func Find() string {
	if override := os.Getenv(EnvFreezeFile); override != "" {
		if _, err := os.Stat(override); err == nil {
			return override
		}
		return ""
	}

	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ReadFile parses the freeze marker at path. Returns nil when the file is
// missing or unreadable.
func ReadFile(path string) *Info {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path comes from Find — an ancestor walk for a fixed filename, or the operator's own BD_MIGRATION_FREEZE_FILE
	if err != nil {
		return nil
	}
	return parse(string(data))
}

// parse reads the marker's optional payload: operator, RFC3339 timestamp, and
// reason, tab-separated on a single line. Every field is optional — an empty
// file is a valid freeze.
func parse(content string) *Info {
	content = strings.TrimSpace(content)
	if content == "" {
		// No recorded timestamp to parse (e.g. an empty marker file, such
		// as one created with `touch`) — leave Timestamp at its zero value
		// rather than fabricating "now": a caller that ever inspects it
		// should not be told the freeze started at check-time.
		return &Info{}
	}
	parts := strings.SplitN(content, "\t", 3)
	info := &Info{}
	if len(parts) >= 1 {
		info.Operator = parts[0]
	}
	if len(parts) >= 2 {
		if t, err := time.Parse(time.RFC3339, parts[1]); err == nil {
			info.Timestamp = t
		}
	}
	if len(parts) >= 3 {
		info.Reason = parts[2]
	}
	return info
}
