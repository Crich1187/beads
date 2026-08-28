package main

// initRemoteCloneDisposition describes how init continues after a remote clone
// attempt. It deliberately owns only the clone result policy; callers retain
// all output, state, and remote-safety decisions.
type initRemoteCloneDisposition uint8

const (
	initRemoteCloneFailed initRemoteCloneDisposition = iota
	initRemoteCloneBootstrapped
	initRemoteCloneFresh
)

// initCloneURL returns the URL init should hand to DOLT_CLONE.
//
// A git-forge URL must reach Dolt in its git+ form. Dolt's dbfactory routes by
// scheme: raw http(s):// goes to the remotesapi client, which speaks Dolt's
// wire protocol at github.com and retries indefinitely (#4421), while
// git+https/git+ssh goes to the git remote factory, which shells out to git
// and fails cleanly. Init has never had bootstrap's forge guard, so a
// configured `https://github.com/org/repo.git` sync.remote was cloned as-is
// straight into the storm path.
//
// Everything else is returned byte-identical. That preserves GH#3339: a
// user-configured Dolt remotesapi endpoint (http://myserver:7007/mydb) must
// never be rewritten to git+http://, and it never classifies as a forge URL.
func initCloneURL(syncURL string) string {
	if isGitCodeRepoURL(syncURL) {
		return normalizeRemoteURL(syncURL)
	}
	return syncURL
}

// runInitRemoteClone runs one remote clone attempt and classifies the result
// for Init. Empty remotes initialize locally; every other clone error is
// returned unchanged.
func runInitRemoteClone(remoteURL string, clone func(string) error) (initRemoteCloneDisposition, error) {
	if err := clone(remoteURL); err != nil {
		if isEmptyRemoteCloneError(err) {
			return initRemoteCloneFresh, nil
		}
		return initRemoteCloneFailed, err
	}
	return initRemoteCloneBootstrapped, nil
}

// initRemoteHostConflict contains the effective remote host configuration
// that embedded Init rejects when server mode is not enabled.
type initRemoteHostConflict struct {
	host         string
	source       string
	includesPort bool
}

// detectInitRemoteHostConflict applies Init's host-precedence rule. An
// environment host overrides config, and ports are only explanatory once a
// remote effective host has made the configuration incompatible with embedded
// mode.
func detectInitRemoteHostConflict(configHost, envHost, configPort, envPort string) *initRemoteHostConflict {
	effectiveHost := configHost
	hostSource := "config.yaml"
	if envHost != "" {
		effectiveHost = envHost
		hostSource = "environment"
	}
	if effectiveHost == "" || isLocalHost(effectiveHost) {
		return nil
	}
	return &initRemoteHostConflict{
		host:         effectiveHost,
		source:       hostSource,
		includesPort: configPort != "" || envPort != "",
	}
}
