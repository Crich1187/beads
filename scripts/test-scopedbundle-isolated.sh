#!/usr/bin/env bash
set -euo pipefail

# Run the scoped-bundle tests without touching host build state or a live Dolt
# endpoint. The input checkout and module cache are read-only; all writes stay
# in the container's private tmpfs, and the container has no network.
repo_root=$(rtk git rev-parse --show-toplevel)
container_name="root-55fr9-13-21-scopedbundle-codex-p7-$$"

cleanup() {
  local container_id
  container_id=$(rtk docker ps -aq --filter "name=^${container_name}$")
  if [[ -n "$container_id" ]]; then
    rtk docker rm -f "$container_id"
  fi
}
trap cleanup EXIT

# The single-quoted inner script expands only inside the container.
# shellcheck disable=SC2016
rtk docker run --rm --name "$container_name" \
  --network none \
  --read-only \
  --tmpfs /tmp:rw,exec,nosuid,nodev,size=8g \
  -e HOME=/tmp/home \
  -e GOPATH=/go \
  -e GOMODCACHE=/go/pkg/mod \
  -e GOCACHE=/tmp/go-build \
  -e GOTOOLCHAIN=auto \
  -e CGO_ENABLED=1 \
  -e 'GOFLAGS=-tags=gms_pure_go,scopedbundle_integration -buildvcs=false' \
	-e SCOPED_BUNDLE_TEST_ADDR=127.0.0.1:13308 \
  -v /usr:/usr:ro \
  -v /lib:/lib:ro \
  -v /lib64:/lib64:ro \
  -v /root/go/pkg/mod:/go/pkg/mod:ro \
  -v /root/go/pkg/sumdb:/go/pkg/sumdb:ro \
  -v "$repo_root":/input:ro \
  --entrypoint /bin/bash \
  postgres:16 \
  -euo pipefail -c \
  'mkdir -p /tmp/home /tmp/go-build /tmp/src /tmp/data/scoped_host
   cp -a /input/. /tmp/src/
   dolt config --global --add user.name scoped-bundle-test >/tmp/config-name.out
   dolt config --global --add user.email scoped-bundle-test@invalid.example >/tmp/config-email.out
   cd /tmp/data/scoped_host
   dolt init >/tmp/init.out 2>/tmp/init.err
   cd /tmp
   dolt sql-server --host 127.0.0.1 --port 13308 --data-dir /tmp/data >/tmp/dolt-server.log 2>&1 &
   server_pid=$!
   stop_server() { kill "$server_pid" 2>/tmp/cleanup.err || true; wait "$server_pid" 2>/tmp/wait.err || true; }
   trap stop_server EXIT
   ready=0
   for _ in $(seq 1 80); do
     if (exec 3<>/dev/tcp/127.0.0.1/13308) 2>/tmp/readiness.err; then exec 3>&-; exec 3<&-; ready=1; break; fi
     sleep 0.25
   done
   if [[ "$ready" -ne 1 ]]; then
     log_size=$(stat -c %s /tmp/dolt-server.log)
     log_hash=$(sha256sum /tmp/dolt-server.log | awk "{print \$1}")
     starts=$(grep -Eic "start|listen|ready" /tmp/dolt-server.log || true)
     errors=$(grep -Eic "error|fatal|panic|fail" /tmp/dolt-server.log || true)
     printf "private_dolt_readiness=FAIL log_size=%s log_sha256=%s startup_patterns=%s error_patterns=%s\n" "$log_size" "$log_hash" "$starts" "$errors"
     exit 1
   fi
   cd /tmp/src
   go test -tags=gms_pure_go,scopedbundle_integration -count=1 -cover ./internal/scopedbundle
   go test -tags=gms_pure_go -count=1 -run "Test(MigrateScoped|DecodeScoped)" ./cmd/bd
   go test -tags=gms_pure_go -count=1 ./internal/storage/issueops ./internal/eventsjournal
   go vet ./internal/scopedbundle ./cmd/bd
   go build -tags=gms_pure_go -o /tmp/bd-scoped ./cmd/bd
   /tmp/bd-scoped migrate scoped --help >/tmp/scoped-help.out
   /tmp/bd-scoped migrate scoped inspect --help >/tmp/scoped-inspect-help.out
   /tmp/bd-scoped migrate scoped export --help >/tmp/scoped-export-help.out
   /tmp/bd-scoped migrate scoped apply --help >/tmp/scoped-apply-help.out
   test "$(grep -Ec "inspect|export|apply" /tmp/scoped-help.out)" -ge 3
   test "$(grep -Ec -- "--map|--id-side" /tmp/scoped-inspect-help.out)" -ge 2
   test "$(grep -Ec -- "--map|--output" /tmp/scoped-export-help.out)" -ge 2
   test "$(grep -Ec -- "--bundle|--expect-current|--actor" /tmp/scoped-apply-help.out)" -ge 3
   shellcheck scripts/test-scopedbundle-isolated.sh
   printf "scoped_bundle_isolated_checks=PASS private_endpoint=container-loopback:13308\n"'
