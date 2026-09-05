# Scoped bundle migration implementation plan

> Bead: `root-55fr9.13.21`
> Base: `ce448cb395dbf9b2784dd37c52f1c126a52670cd`
> Design: `engdocs/superpowers/specs/2026-09-05-scoped-bundle-migration-design.md`

## Constraints

All tests run in an existing, network-disabled container with source/module
inputs read-only and all writable state on private tmpfs. Integration tests
launch one Dolt SQL server on container loopback without publishing a host
port or mounting the Docker socket. No live database or service is used.

## Tasks

### 1. Pin the bundle and mapping contracts

- Add failing unit tests for explicit manifest cardinality/digest validation,
  deterministic encoding, reference mapping, collisions, and closed-set
  rejection. Retain 17-pair coverage, add 18- and 19-pair coverage, and prove
  stale 17-vs-18 and 18-vs-19 rejection.
- Implement only the immutable bundle/model/validation layer needed to pass.
- Run the new package tests in the isolated unit container.

### 2. Pin schema compatibility and read-only export

- Add failing SQL-mock tests for v53/current-compatible projections and
  rejection of incompatible or non-default unrepresentable columns.
- Implement schema inspection, deterministic scoped reads, and export.
- Prove export performs no DDL and produces stable reruns.

### 3. Pin atomic apply and journal behavior

- Add failing tests for exact-current preconditions, exact rerun no-op,
  collision/mismatch refusal, and transaction rollback on injected failure.
- Implement apply in one transaction, using existing `issueops` journal scope
  and emitters when journal activation is enabled.
- Prove disabled/v53 and enabled/current journal paths independently.

### 4. Exercise a private real Dolt endpoint

- Launch an explicit Dolt SQL server at a container-loopback endpoint.
- Create synthetic v53-compatible and current/journal-enabled databases.
- Run explicit-manifest 17-, 18-, and 19-pair migration coverage, including
  stale 17-vs-18 and 18-vs-19 rejection, plus delete/restore rehearsal, failure, collision,
  closed-set, and unrelated-newer-write controls with clean exits.

### 5. Wire the bounded CLI

- Add `bd migrate scoped inspect|export|apply` with store-open bypass.
- Reuse workspace connection resolution and accept only map/bundle paths,
  expected digest, ID side, output path, and actor.
- Add command contract tests that prove schema migrations are not invoked and
  missing preconditions fail before a write.

### 6. Verify and hand off

- Run focused tests, the narrow applicable regression suites, formatting,
  static checks, and UBS on exact changed files in isolation.
- Record broad-baseline failures and skipped coverage separately; make no green
  claim for them.
- Commit with required signature, push only the owned branch, verify remote
  exact SHA, and write an immutable handoff for different-family Gate 3/4.
