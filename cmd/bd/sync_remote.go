package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/doltremote"
)

// resolveSyncRemote returns the effective sync remote URL.
// Resolution order:
//  1. sync.remote (primary — any Dolt-compatible remote URL)
//  2. sync.git-remote (deprecated fallback)
//  3. "" (not configured)
func resolveSyncRemote() string {
	if v := config.GetString("sync.remote"); v != "" {
		return v
	}
	return config.GetString("sync.git-remote")
}

// resolveSyncRemoteFromDir is like resolveSyncRemote but reads from a
// specific beads directory's config.yaml. Used by context_cmd, doctor,
// and other paths that operate on a resolved beads dir rather than CWD.
func resolveSyncRemoteFromDir(beadsDir string) string {
	if v := config.GetStringFromDir(beadsDir, "sync.remote"); v != "" {
		return v
	}
	return config.GetStringFromDir(beadsDir, "sync.git-remote")
}

// commitBeadsConfig stages .beads/config.yaml and commits it.
// Silently no-ops if the file is clean or the commit fails (e.g. hooks,
// nothing to commit). Used by bd dolt remote add/remove to keep the
// working tree clean after persisting sync.remote.
func commitBeadsConfig(msg string) {
	commitBeadsConfigForActiveRepo(context.Background(), msg)
}

func commitBeadsConfigForActiveRepo(ctx context.Context, msg string) {
	rc, err := beads.GetRepoContext()
	if err != nil {
		return
	}
	addCmd := rc.GitCmd(ctx, "add", ".beads/config.yaml")
	if err := addCmd.Run(); err != nil {
		return
	}
	commitCmd := rc.GitCmd(ctx, "commit", "-m", msg)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "nothing to commit") {
			fmt.Fprintf(os.Stderr, "Warning: failed to commit config change: %v\n", err)
		}
	}
}

// normalizeRemoteURL converts a remote URL to a Dolt-compatible format.
// Dolt-native URLs (dolthub://, file://, aws://, gs://, git+...) are
// returned as-is. Git URLs (https://, ssh://, git@...) are converted
// via gitURLToDoltRemote. Unknown schemes are returned as-is and let
// dolt clone decide.
func normalizeRemoteURL(url string) string {
	return doltremote.Normalize(url)
}

// doltRemoteURL returns the form of remote that bd hands to Dolt — for
// DOLT_CLONE, for DOLT_REMOTE('add', ...), and therefore for DOLT_PUSH.
//
// A git-forge URL must reach Dolt in its git+ form. Dolt's dbfactory routes by
// scheme: raw http(s):// goes to the remotesapi client, which speaks Dolt's
// wire protocol at github.com and retries indefinitely (#4421), while
// git+https/git+ssh goes to the git remote factory, which shells out to git
// and fails cleanly.
//
// Everything else is returned byte-identical. That preserves GH#3339: a
// user-configured Dolt remotesapi endpoint (http://myserver:7007/mydb) must
// never be rewritten to git+http://, and it never classifies as a forge URL.
//
// This is the single owner of that routing rule. bd init and bd bootstrap must
// both call it: if they disagree about the URL derived from one committed
// sync.remote, that is the #5743 class of skew all over again.
func doltRemoteURL(remote string) string {
	if isGitCodeRepoURL(remote) {
		return normalizeRemoteURL(remote)
	}
	return remote
}

// redactRemoteURL strips credentials from a remote URL so it is safe to echo
// into errors, hints, logs and JSON. CI commonly configures sync.remote as
// https://x-access-token:<token>@github.com/org/repo.git, and the clone funnel
// already scrubs userinfo before reporting (versioncontrolops.sanitizeURL);
// diagnostics must not be the hole in that convention.
//
// HTTP(S) userinfo is transport credentials and is dropped whole. SSH userinfo
// selects the remote account (git@host is not a secret and is needed for the
// hint to be runnable), so only a password component is dropped.
func redactRemoteURL(raw string) string {
	sep := strings.Index(raw, "://")
	if sep < 0 {
		// scp-style (git@host:path) or a bare path: no place for a password.
		return raw
	}
	scheme, rest := raw[:sep], raw[sep+3:]
	authority, tail := rest, ""
	if slash := strings.Index(rest, "/"); slash >= 0 {
		authority, tail = rest[:slash], rest[slash:]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return raw
	}
	userinfo, host := authority[:at], authority[at+1:]
	if isDoltSSHScheme(scheme) {
		if colon := strings.Index(userinfo, ":"); colon >= 0 {
			userinfo = userinfo[:colon]
		}
		return scheme + "://" + userinfo + "@" + host + tail
	}
	return scheme + "://" + host + tail
}

func isDoltSSHScheme(scheme string) bool {
	return scheme == "ssh" || scheme == "git+ssh"
}
