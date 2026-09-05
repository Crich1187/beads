# Scoped bundle migration and recovery design

## Status and authority

Coordinator technical design approval was recorded for `root-55fr9.13.21` on
2026-09-05. This document makes that approved design executable. It is not a QA
verdict or a waiver of the later independent review gates.

## Objective

Provide a narrowly scoped, schema-preserving way to copy or restore an exact
closed set of Beads rows between compatible Dolt databases. Each operation is
bound to an explicit reviewed Research ID-pair manifest, its expected
cardinality, and its digest; fixtures use synthetic data.
The operation must preserve issue fields, labels, comments, dependencies, and
human audit events without changing their authors, actors, timestamps, IDs, or
content.

The same bundle is usable in two situations:

1. applying a freshly frozen source set to its mapped destination IDs; and
2. restoring that exact set after a separately authorized scoped deletion.

Nothing in this feature performs the deletion itself, changes a schema, starts
a server, pushes a remote, or selects a live target.

## Operator interface

The feature is a child of the existing `bd migrate` command:

```
bd migrate scoped inspect --map MAP.json --id-side source|target
bd migrate scoped export --map MAP.json --output BUNDLE.json
bd migrate scoped apply --bundle BUNDLE.json --expect-current SHA256 --actor ACTOR
```

`migrate scoped` opts out of the normal writable store-open path. It connects
to the already configured Dolt SQL endpoint without running Beads schema
migrations. The active workspace still determines the database; there is no
DSN, password, token, or server address flag on this command.

`inspect` prints a deterministic SHA-256 of the selected scoped rows and row
counts. `export` writes a versioned JSON bundle. `apply` requires the exact
pre-mutation target SHA-256. It first recognizes an exact already-applied state
as a no-op; otherwise any precondition drift fails before mutation.

## Mapping contract

The mapping document is versioned and declares explicit source and target
namespace prefixes, its expected cardinality, and its digest. Empty IDs,
wildcards in a prefix, an ID outside its declared prefix, duplicate sources,
duplicate targets, a pair count different from the declared cardinality, or a
digest mismatch are errors. Prefixes are used only for a read-only closed-set
census; they never select mutation targets. Every mutation ID and in-scope
foreign key must resolve through the reviewed pair list. No prefix inference,
wildcard mutation, automatic scope expansion, or textual search-and-replace is
allowed.

The production manifest is expected to carry these suffixes:

```
001, API-120, Adjudication-100, Campaigns-040, Campaigns-045, Campaigns-046,
Campaigns-047,
Constitution-020, Eval-090, Explorer-070, Infrastructure-130, Ledger-050,
Platform-010, Playbook-110, Quality-140, Referee-030, ResearchGraph-060,
Validation-150, Workers-080
```

Tests retain the original equivalent 17-pair synthetic manifest, add 18- and
19-pair manifests, and prove stale 17-vs-18 and 18-vs-19 source censuses fail
closed. The production manifest cardinality is never frozen in code: final
execution requires a fresh source census, reviewed pair set, cardinality, and
digest. Tests never export live data.

## Bundle format and integrity

The bundle has a fixed format identifier, format version, source schema
version, mapping, writable-column schema descriptors, ordered table rows, per-table counts,
source-state digest, mapped desired-state digest, and a bundle digest. The
bundle digest covers every field except itself. JSON encoding is deterministic:
columns and rows are sorted, cells are explicit null-or-text values, and no Go
map is part of a hashed representation.

The five transferred tables are `issues`, `labels`, `comments`,
`dependencies`, and `events`. Generated columns are inspected for target
compatibility but omitted from bundle rows and never written. Primary IDs
for comments, dependencies, and events are preserved. Only issue reference
columns are mapped.

## Schema compatibility

The minimum accepted Beads schema is v53. The command reads
`schema_migrations` and `information_schema`; it never calls a migration or
executes DDL.

For each transferred table:

- required identity/reference columns must exist with compatible SQL families;
- a source column absent at the target is representable only when every scoped
  source value is null or equal to that source column's default;
- a target-only writable column is representable only when it is nullable or
  has a default, and any existing scoped value is null/default;
- generated columns are validated but omitted from writes; and
- any missing required table, incompatible type family, or non-default
  unrepresentable value aborts before the transaction mutates data.

This permits the v53/current common projection while refusing silent data loss.

## Closed-set and collision checks

Export and apply both inspect every dependency incident to the selected issue
set. A dependency whose issue endpoint is outside the set, whose typed target
cannot be mapped, or whose target uses the wisp/external plane is rejected.

Before mutation, apply checks all preserved row IDs globally. A comment,
dependency, or event ID already owned by a different issue is a collision and
fails closed. An identical row already attached to the mapped issue is
idempotent; a same-ID/different-content row is not.

Target comments and audit events must be a subset of the desired set unless
they match it exactly. The operation never silently deletes destination-only
history. This is what makes a stale bundle fail rather than erase a post-freeze
comment.

## Atomic apply and journal semantics

All precondition checks that protect the mutation are repeated inside one SQL
transaction. Apply then reconciles only mapped issue IDs and their incident
rows. Unrelated rows are neither selected for update nor deleted. Any failure
rolls the complete transaction back.

On v53, no durable mutation journal exists, so the transaction preserves the
human `events` table only. On a workspace where `events-journal` is enabled,
apply uses the existing `issueops` transaction journal scope and emitters in
the same transaction:

- create/update snapshots for issue state and labels;
- dependency add/remove records for actual edge deltas; and
- comment records only for newly inserted comments.

Destination-only comments are unrepresentable and fail closed because the
journal vocabulary has no comment-delete operation. Exact reruns make no data
change and append no journal rows.

## Recovery rehearsal

The synthetic recovery test exports a bundle selected by an explicit manifest,
records the exact scoped digest and expected cardinality, deletes precisely
those fixture rows in a transaction, makes a newer write to an unrelated
control row, and applies the bundle using the post-delete digest. Equality is
checked across all five tables. The unrelated row and its newer write must
remain byte-for-byte unchanged.

Induced preflight and mid-transaction failures prove zero partial writes.
Rerunning the exact apply proves both data and journal idempotence.

## Non-goals

- No live Research or host-root execution.
- No delete command or authorization bypass.
- No server lifecycle, remote push, bootstrap, credential, or deployment work.
- No schema migration or automatic repair.
- No generic full-database backup/restore replacement.
