# Pipe ACLs — identities, permissions, enforcement

Status: **phases 0-2 landed** (2026-08-18); phases 3-4 planned.

| Phase | What | Status |
|---|---|---|
| 0 | Prerequisites — key principals, presence isolation | landed |
| 1 | Principals — org roles, service accounts | landed |
| 2 | ACL store, evaluator, CLI + HTTP surface | landed |
| 3 | Enforcement — HTTP + NATS credentials | planned |
| 4 | Key attenuation | planned |

**Nothing is enforced yet.** Through phase 2 an ACL describes intent and the
CLI/HTTP surfaces honour it, but the minted NATS credential is still
`pub: >, sub: >` — a hand-rolled NATS client is not stopped. Phase 3 is
what makes it real.

Picks up the thread `docs/AUTH-V2.md` deferred as "Phase 3.6 — per-user
role-scoped JWTs + HTTP RBAC middleware". That entry assumed roles were
the unit of authorization; this plan replaces it with per-pipe grants,
because the interesting cases (an agent that may write to your inbox but
not read your terminal) don't decompose into org-wide roles.

## Goals

1. A pipe can be readable, writable, or administered by a named set of
   principals rather than by everyone in the org.
2. Agents get their own identity, distinct from the human who spawned
   them, with narrower rights than that human.
3. Enforcement holds against a hand-rolled NATS client, not just against
   the `ppz` CLI.

## Non-goals

- Cross-org sharing. A grant names a principal in the same account; the
  NATS account boundary from Auth V2 Phase 3.5 stays the tenancy edge.
- ~~Audit log of grant changes.~~ **Revised** — this was a non-goal only
  because no audit table existed. PR #191 (`ppz pipe set`) lands
  `audit_events`, so grant/revoke should emit audit events. Folded into
  Phase 3; see "Interaction with #191" below.
- Content-level policy (redaction, DLP, per-message rules). ACLs gate
  pipes, not payloads.
- Deny rules. See "Why there are no deny rules" below — the derived
  defaults make them unnecessary, and adding them later is additive.

---

## The model

### Principals

One subject type for every grant: a row in `users`.

| Kind | `users.mode` | Created by | Notes |
|---|---|---|---|
| Human | `github` / `internal` | OAuth signup | Today's users, unchanged |
| Service account | `service` | `ppz svc create` / GUI | Org-owned; an agent's own identity |
| `@everyone` | `internal` | Seeded, fixed UUID | Pseudo-principal meaning "all members of this account" |

Service accounts are ordinary principals: they hold grants, they own
handles, they're attributed on `ppz who` and on every message they
publish. They differ from humans only in how they authenticate (key
only, no OAuth) and in being scoped to one account.

`@everyone` is seeded with a fixed UUID exactly the way the
`unauthenticated` placeholder already is (`0001_init.sql`), which keeps
the FK and the uniqueness constraint on `acl_grants` honest.

### Permissions

Three capabilities. **`read` and `write` are independent** — neither
implies the other. `admin` implies both, plus the right to change the
pipe's ACL, retention and existence.

| Perm | Grants |
|---|---|
| `read` | Consume history and tail live traffic |
| `write` | Publish |
| `admin` | `read` + `write` + grant/revoke on this pipe, change retention, destroy |

Write-without-read is the load-bearing case (`<handle>.inbox`: anyone
may send to alice, only alice may read what arrived), and it is
enforceable rather than merely declarable — see "Why the split is free"
under Phase 3.

### Objects — the selector grammar

A selector matches a pipe's **subject path**: the string
`natsubj.BuildSubject` produces, minus the account prefix.

| Form | Matches |
|---|---|
| `alice.inbox` | exactly that pipe |
| `alice.*` | one token — every pipe directly under `alice` |
| `alice.**` | one or more trailing tokens — everything under `alice` |
| `*.inbox` | every handle's inbox |
| `**` | every pipe in the account |

`*` and `**` map 1:1 onto NATS `*` and `>`, which is what makes the JWT
compiler in Phase 3 close to mechanical.

The manifold/collar ambiguity is inherited deliberately, not invented:
`natsubj` already documents that `acct.X.pipe` is indistinguishable at
the wire level between a manifold and a source segment
(`internal/natsubj/subjects.go`, `BuildSubject`). Selectors match the
path, so they inherit exactly that behaviour and no new ambiguity is
introduced.

### Defaults — derived, never stored

**The collar is the ownership boundary.** Everything under a handle
belongs to that handle's principal. The two exceptions are the pipes
whose entire purpose is cross-principal traffic, and they are duals of
each other. Uncollared pipes are shared org space.

| Pipe | `read` | `write` |
|---|---|---|
| `<handle>.inbox` | handle owner | **everyone** |
| `<handle>.heartbeat` | **everyone** | handle owner |
| `<handle>.stdin` / `stdout` / `stdctrl` / `system` | handle owner | handle owner |
| `<handle>.<user-created>` | handle owner | handle owner |
| uncollared `<manifold>.<name>` | everyone | everyone |

Handle ownership is `sources.created_by_user_id`, which already exists.

Note the inventory: `broadcast` was removed pre-launch (locked decision
#16), so what is actually provisioned is `inbox` for `message` sources and
`heartbeat`/`inbox`/`stdin`/`stdout`/`stdctrl`/`system` for `pty` sources
(`internal/daemon/handlers.go`, `pipesForKind`).
`natsubj.AutoProvisionedPipes` must *cover* that set — it was missing
`heartbeat` and `system` until Phase 0c — but is deliberately a superset
and retains `broadcast`. See "Interaction with PR #191" above.

### Evaluation

```
eff(principal P, pipe π) -> set of {read, write, admin}

1. P is the account owner (accounts.owner_user_id)
   or account_members.role = 'admin'         -> {read, write, admin}, done.

2. start with defaults(π, P) from the table above

3. union in every acl_grants row where
        principal_id ∈ {P, @everyone}
    and selector matches π's subject path

4. if admin ∈ result, add read and write

5. if the presenting credential carries key_scopes rows,
   intersect (Phase 4)
```

Org owner and admin are computed at step 1 and never stored as grant
rows, so no revoke can lock an owner out of their own org.

### Why there are no deny rules

An earlier draft stored the default as an `@everyone → ** → read,write`
row and expressed lockdown by subtracting from it. That needs deny rows,
and deny rows need a precedence rule (otherwise `--private` on a pipe
also locks out its creator).

Deriving defaults from `(collar, pipe name)` removes the need entirely:
there is no stored blanket grant to subtract from, so **every stored row
is an allow that widens a default**. No deny, no precedence tiers, no
tie-break. If a real "everyone except bob" case turns up later, deny
rows can be added additively without reworking anything here.

---

## Decisions (locked)

| Decision | Choice |
|---|---|
| Permission lattice | `read` ⊥ `write`; `admin` ⇒ both + manage |
| Defaults | Derived from collar + pipe name (table above), never stored |
| Deny rules | None in v1 |
| Grant subject | A `users` row; one type for humans, services and `@everyone` |
| Agent identity | Service accounts (`users.mode='service'`), org-owned |
| Key scoping | Keys may carry an intersect-only narrowing of their principal |
| Handle binding | Publishing under a handle requires owning it or holding `write` |
| Org roles | New `account_members.role` ∈ `owner`/`admin`/`member` |
| Org owner/admin | Implicit `admin` on `**`, computed, not revocable |
| Enforcement | HTTP control plane **and** NATS credentials |
| `ppz ls` | Lists pipes you cannot read, marked; suppresses their contents |
| Pipe roster | Visible to any principal holding any access on that pipe |
| Every surface | Reports effective access **with provenance**, never the raw grant table |
| Selector grammar | Subject path with `*` (one token) / `**` (rest) |

## Interaction with PR #191 (`ppz pipe set` — retention + audit)

#191 is ahead of this work in the queue and overlaps it in twelve files.
Most are mechanical (both add a case to the same switch, a row to the
same help table, a line to `reset.sh`). Three items are not:

**Migration numbering.** #191 claims `0006_audit_events.sql`. This work
renumbered to `0007_api_key_principal` / `0008_principals` /
`0009_acl` to leave it clear. Relative order is preserved — principals
must precede `acl_grants`, which FKs `users` and expects the `@everyone`
seed.

**`AutoProvisionedPipes`.** Both branches independently found the same
bug (the set was missing `system` and `heartbeat`), and disagreed on
`broadcast`: this work removed it, #191 kept it deliberately because the
`pipe destroy` glob consumer treats the set as a skip-list, where
retaining a dead name costs nothing. #191's reading is the better one and
has been adopted here — the set is a **superset that must cover** what is
provisioned, not an exact mirror. Both consumers are safe under a
superset; neither is safe under a subset.

**Two things #191 makes newly necessary, both Phase 3 scope:**

- `ppz pipe set` needs an ACL gate. Its own doc comment notes it
  deliberately reaches reserved auto-pipes (`inbox`, `stdout`) that
  `pipe create` can never name — and changing a pipe's retention is
  exactly what `admin` is defined to cover. It is currently ungated.
- `auditPipe(ctx, key, …)` must attribute via `key.Actor()`, not the
  key's creator. Same argument as row attribution: a service-account key
  is minted by a human and acts as the bot, so logging the human makes
  the audit trail wrong precisely where it matters most.

## Build protocol

Unchanged from `docs/AUTH-V2.md`: write every test red first, **stop and
surface the RED list for review**, get explicit ack, then implement to
green and commit each green increment. No implementation lands without a
test that flips red → green.

---

## Phase 0 — Prerequisites

Two existing behaviours make ACLs unenforceable. Both are independent of
the ACL model and both are bugs on their own terms, so they land first.

### 0a. API keys must resolve to a principal

Today `AuthedCaller.UserID` is `uuid.Nil` on the API-key path
(`internal/server/auth_bearer.go`). A V1 key is functionally "full access
to this org, attributed to nobody" — `created_by_user_id` is used only to
stamp rows it creates. There is no principal to hang a grant on.

```sql
ALTER TABLE api_keys ADD COLUMN principal_user_id uuid REFERENCES users(id);
UPDATE api_keys SET principal_user_id = created_by_user_id WHERE principal_user_id IS NULL;
ALTER TABLE api_keys ALTER COLUMN principal_user_id SET NOT NULL;
```

`resolveBearer` populates `UserID` on both paths. `requireAPIKey`'s
synthetic `db.APIKey` keeps working unchanged.

### 0b. Heartbeats must leave the org firehose

`subscribeOrgHeartbeats` core-subscribes to `<account>.>` and filters for
a `.heartbeat` suffix client-side (`internal/daemon/heartbeat_subscriber.go`).
Live JetStream publishes fan out to core subscribers, so **any principal
holding that subscription sees every message published in the org in real
time**, regardless of how tightly its JetStream permissions are drawn.
Read ACLs are bypassable in one line of client code until this moves.

Fix: relocate heartbeats to a dedicated prefix so a single subscription
catches them all and the ACL can grant them separately from everything
else under the handle.

```
<account>.<manifold?>.<handle>.heartbeat   ->   <account>._presence.<manifold?>.<handle>
```

Daemon subscribes to `<account>._presence.>`. Stream name moves with it.
Wire change; acceptable on the same footing as the Phase 3.5 grammar
change, and there is no history worth preserving on a presence pipe.

*Alternative considered:* make presence core-only (no stream). Cheaper
still, and durability buys nothing for a liveness signal — but it changes
`ppz ls` output, which currently shows `heartbeat` with a message count.
Deferred; the prefix move is enough to unblock ACLs.

### 0c. Cleanup

`natsubj.AutoProvisionedPipes` still lists `broadcast`, removed pre-launch.
Drop it so the ACL default table has one source of truth.

### Phase 0 RED tests

**Unit**
- `TestResolveBearer_APIKey_PopulatesUserID`
- `TestResolveBearer_OAuth_PopulatesUserID` (regression)
- `TestPresenceSubject_RoundTrip` — build/parse `_presence` paths at root and nested manifolds.

**Integration**
- `TestHeartbeatSubscriber_DoesNotSeePipeTraffic` — publish to a normal pipe, assert the presence subscription never fires. This is the test that proves the leak is closed.

**E2E**
- `tests/agent/heartbeat-presence-subject` — `ppz who` still populates after the move.
- `tests/status/api-key-attributes-to-principal` — a row created via an API key attributes to the key's principal.

### Phase 0 effort: ~1 day.

### Phase 0 implementation notes (landed)

**Presence routing lives in the builders, not the call sites.**
`BuildSubject` and `BuildStreamName` return the presence shape when the
pipe name is `heartbeat`. Provisioning (`ensurePipeStream`), the send
target resolver, the read path, `ppz ls` and the GUI chat roster all
derive their subject and stream name from those two functions — four
call sites that would eventually drift if each branched on its own.
Presence streams take a `presence_` name prefix rather than `pipe_` so
they can never collide with a source legitimately named `presence`.

`heartbeat` joins `ReservedPipeNames` as a consequence: a user-created
pipe of that name would now route into another source's presence stream.

**`AutoProvisionedPipes` becomes the single source of truth**, reconciled
to `{inbox, heartbeat, stdin, stdout, stdctrl, system}`. It previously
listed `broadcast` (removed pre-launch) and omitted `heartbeat` and
`system`. Widening it also fixed a latent bug in `ppz pipe destroy`
glob expansion, which used the set to skip auto-provisioned pipes and
therefore would have happily matched `alice.heartbeat`.

**Follow-ups this surfaced, for Phase 1:**

- `requireAPIKey` still hands `CreatedByUserID` downstream for row
  attribution. Phase 0 makes that harmless (principal == creator), but
  once a human can mint a service-account key the created rows must
  attribute to `PrincipalUserID` or the service's work is credited to
  the human.
- `db.Source.Pipes()` and the daemon's `pipesForKind` agree with
  `AutoProvisionedPipes` by test, not by construction. Deriving them
  would be better than pinning them.
- `tests/lib/reset.sh` re-seeds `account_members` with a bare `INSERT`;
  it needs the `role` column or every scenario resets roles to default.
- Existing `pipe_<org>_<handle>_heartbeat` streams are orphaned by the
  rename. Harmless per the wipeable-prod posture, and `reset.sh` clears
  JetStream between e2e scenarios.

---

## Phase 1 — Principals

### Schema — `0006_principals.sql`

```sql
-- Org roles. accounts.owner_user_id stays the authority for 'owner';
-- this column distinguishes admin from member.
ALTER TABLE account_members ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'member';
ALTER TABLE account_members ADD CONSTRAINT account_members_role_check
    CHECK (role IN ('owner','admin','member'));

-- InsertAccount does not currently add a member row for the owner.
-- Backfill so the members table is complete and the GUI can render roles.
INSERT INTO account_members (account_id, user_id, role)
SELECT id, owner_user_id, 'owner' FROM accounts WHERE owner_user_id IS NOT NULL
ON CONFLICT (account_id, user_id) DO UPDATE SET role = 'owner';

-- Service accounts.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_mode_check;
ALTER TABLE users ADD CONSTRAINT users_mode_check
    CHECK (mode IN ('github','internal','service'));
ALTER TABLE users ADD COLUMN IF NOT EXISTS service_account_id uuid
    REFERENCES accounts(id) ON DELETE CASCADE;
ALTER TABLE users ADD CONSTRAINT users_service_owner_check
    CHECK ((mode = 'service') = (service_account_id IS NOT NULL));

-- @everyone pseudo-principal, fixed UUID, same pattern as the
-- 'unauthenticated' seed row in 0001_init.sql.
INSERT INTO users (id, username, email, mode)
VALUES ('00000000-0000-0000-0000-000000000002', 'everyone', 'everyone@local', 'internal')
ON CONFLICT (username) DO NOTHING;
```

**Wart:** `users.username` is globally unique, so two orgs can't both have
a `builder-bot`. Service rows store `username` as `<org>/<name>` and
display the bare name in-org. Ugly but contained, and it avoids
relaxing a constraint every other lookup depends on.

### Routes

| Method · Path | Behaviour |
|---|---|
| `POST /api/v1/svc` | Create a service account in the caller's org. Requires org admin. |
| `GET /api/v1/svc` | List service accounts. |
| `DELETE /api/v1/svc/{name}` | Delete; cascades its keys and grants. Org admin. |
| `POST /api/v1/svc/{name}/keys` | Mint a key whose principal is the service account. |
| `POST /orgs/{id}/members/{uid}/role` | Set `admin`/`member`. Owner only. |

`RoleInOrg` gains `OrgRoleAdmin` between owner and member. Every existing
owner-only gate (`handlers_owner_gates.go`) is reviewed: key revoke,
member add/remove and invites become owner-or-admin; ownership transfer
stays owner-only.

### Phase 1 RED tests

**Unit**
- `TestRoleInOrg_Admin` / `_Member` / `_Owner` / `_NonMember`
- `TestRoleInOrg_OwnerWithoutMemberRow` — regression on the backfill: owner resolves even if `account_members` is missing a row.
- `TestServiceAccount_RequiresOwningOrg` — CHECK constraint rejects `mode='service'` with NULL `service_account_id`, and a human row with one set.
- `TestServiceAccount_NameScopedPerOrg` — same bare name in two orgs.

**Integration**
- `TestCreateServiceAccount_AsAdmin_200` / `_AsMember_403`
- `TestServiceAccountKey_AuthenticatesAsService` — a key minted for `svc:builder` resolves to the service principal, not to the human who minted it.
- `TestSetRole_OwnerOnly`

**E2E**
- `tests/org/svc-create-and-key-authenticates`
- `tests/org/role-admin-can-revoke-key`
- `tests/org/role-member-cannot-revoke-key`

### Phase 1 effort: ~2 days.

### Phase 1 implementation notes (landed)

- `RoleInOrg` reads `account_members.role` for the admin tier but keeps
  `accounts.owner_user_id` as the authority for ownership, so a bad row
  can never leave an org without an owner. A stale `role='owner'` on
  someone who is not the account owner is treated as admin.
- Ordering lives in `internal/acl` and `server.OrgRole` delegates to it,
  so the HTTP gates and the ACL evaluator cannot disagree.
- `handleGUIRevokeKey` previously permitted admin by *falling out of a
  switch* with no matching case — the right answer for the wrong reason.
  Now explicit.
- `db.UsernamesByIDs` resolves through `DisplayName()`. Without it the
  org-scoped storage form leaked into `ppz ls`, showing
  `alpha/builder-bot` where the user typed `builder-bot`.
- Attribution now flows through `db.APIKey.Actor()` (principal, not
  minter) at every create site — the follow-up phase 0 flagged.

---

## Phase 2 — ACL store, evaluator, and surface

Nothing is enforced in this phase. The mutation surface must ship
**before** enforcement, or Phase 3 lands and locks everyone out with no
way to open anything up.

### Schema — `0007_acl.sql`

```sql
CREATE TABLE IF NOT EXISTS acl_grants (
    id            uuid PRIMARY KEY,
    account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    principal_id  uuid NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    selector      text NOT NULL,
    perm          text NOT NULL CHECK (perm IN ('read','write','admin')),
    granted_by    uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, principal_id, selector, perm)
);

CREATE INDEX IF NOT EXISTS acl_grants_account_idx   ON acl_grants (account_id);
CREATE INDEX IF NOT EXISTS acl_grants_principal_idx ON acl_grants (account_id, principal_id);

-- Bumped on any grant / revoke / membership / source / pipe change.
-- Drives credential invalidation in Phase 3.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS acl_generation bigint NOT NULL DEFAULT 0;
```

### New package: `internal/acl`

Pure, no DB, no NATS — so the matcher and evaluator are table-testable.

```go
type Perm uint8                    // bitset: Read | Write | Admin
type Selector string

func Match(sel Selector, subjectPath string) bool
func Intersect(a, b Selector) []Selector      // Phase 4
func Defaults(collar, pipeName string, isHandleOwner bool) Perm
func Evaluate(role OrgRole, def Perm, grants []Grant, path string) Decision
```

**`Evaluate` returns provenance, not a bare bitset.** Every surface in
the next section has to answer "why" as well as "what", and the reason
is only knowable here:

```go
type Decision struct {
    Perm Perm
    Why  map[Perm]Reason   // per-capability: which rule granted it
}

type Reason struct {
    Kind    ReasonKind      // Default | Grant | HandleOwner | OrgRole | KeyScope
    Grant   *Grant          // set when Kind == Grant
    Detail  string          // "inbox is write-open", "owns handle alice"
}
```

This is not decoration. With defaults derived rather than stored, most
access has **no** row behind it — a UI that renders `acl_grants` shows
an almost-empty table and tells the operator that nobody can reach
`alice.inbox`, when in fact every member can write to it. Provenance is
what makes the difference between the two visible.

`Decision.Perm` alone is what the JWT compiler consumes; the surfaces
consume `Why`.

### Mutation verbs

```
ppz pipe acl grant  <pipe|selector> <principal> <read|write|admin>[,…]
ppz pipe acl revoke <pipe|selector> <principal> [read|write|admin|all]
```

Principals are spelled bare (`alice`, `builder-bot`) and resolved within
the account; `user:` / `svc:` disambiguate a collision; `@everyone` is the
pseudo-principal. Selectors are accepted wherever a pipe is:

```
ppz pipe acl grant 'alice.**' builder-bot read
ppz pipe acl grant alice.notes @everyone read
ppz pipe acl revoke alice.stdout bob all
```

### Visibility surfaces

Three audiences ask three different questions, and every answer carries
its provenance for the reason above.

**By-pipe — "who can touch this?"**

```
$ ppz pipe acl ls alice.stdout
PRINCIPAL    R  W  A  VIA
alice        ✓  ✓  ✓  handle owner (default)
foo          ✓  ✓  ✓  org owner
bob          ✓  ·  ·  grant by alice · 2026-08-14
```

Visible to **any principal holding any access on that pipe**. Coordination
needs it — an agent that gets denied has to know who to ask — and it
leaks little, since handle and pipe names are already org-visible via
`ppz ls`. A principal with no access gets `E_PIPE_FORBIDDEN`.

**By-principal — "what can this agent reach?"** The auditing view, and
the one to run after handing out a key.

```
$ ppz acl ls --principal builder-bot
PIPE              R  W  A  VIA
builder-bot.**    ✓  ✓  ✓  handle owner (default)
*.inbox           ·  ✓  ·  default — inbox is write-open
*.heartbeat       ✓  ·  ·  default — presence is read-open
<uncollared>      ✓  ✓  ·  default — org-shared space
ops.deploy-log    ✓  ✓  ·  grant by foo · 2026-08-12
```

Pivoting on `@everyone` answers "what is exposed to the whole org",
which is the audit query that matters most.

Bare `ppz acl ls` lists the account's stored grants — cheap and exact.
`--effective` expands derived defaults too.

**Self — "what can I do, and why not?"** Passively this is the `ppz ls`
perms column; actively it is the verb that answers the support question
this feature will generate most often.

```
$ ppz acl whoami alice.stdout
alice.stdout — you are 'bob'
  read   ✓  grant from alice · 2026-08-14
  write  ✗  no grant; default for <handle>.stdout is owner-only
  admin  ✗  not the handle owner, not an org admin

  to get write: ppz pipe acl grant alice.stdout bob write
  runnable by:  alice (handle owner), foo (org owner)
```

Printing the exact remediation command **and who is able to run it** is
what makes a denial actionable by an agent: it can ask the right
principal over that principal's inbox instead of failing opaquely. Bare
`ppz acl whoami` gives the by-principal table above, scoped to the
caller.

`--json` on all three. Agents are the primary consumer, and a table
scraped by regex is a bug waiting to happen.

### GUI

The same two pivots, because an admin arrives from both directions.

- **By-pipe**: an ACL panel on the existing pipe page (`/orgs/{id}/sources/{handle}/pipes/{pipe}` and the uncollared variant) — effective access with provenance, plus grant/revoke controls for principals holding `admin`. Same visibility rule as the CLI.
- **By-principal**: a new `access` org tab beside pipes / chat / users / keys, listing the account's stored grants, service accounts and member roles, with a per-principal drill-down that computes effective access on demand. `orgTabs` in `handlers_gui.go` gains the entry.

The tab deliberately does **not** precompute effective access for every
principal × every pipe — that is O(principals × pipes) and would be
recomputed on every page load. Stored grants render exactly; the
expensive view is one click deep and scoped to one principal.

### `ppz ls` marking

New perms column rendering `rwa` / `rw-` / `r--` / `-w-` / `---`. Pipes
you cannot read stay listed — names and owners are org-visible so agents
can discover a pipe and ask for access — but their preview and message
counts are suppressed.

### Phase 2 RED tests

**Unit (`internal/acl`)**
- `TestMatch_Literal` / `_SingleToken` / `_MultiToken` / `_RootWildcard`
- `TestMatch_DoesNotSpanTokens` — `alice.*` must not match `alice.a.b`.
- `TestDefaults_InboxWriteOnlyForOthers`
- `TestDefaults_InboxReadForHandleOwner`
- `TestDefaults_HeartbeatReadForEveryone`
- `TestDefaults_StdioOwnerOnly`
- `TestDefaults_UserCreatedCollaredOwnerOnly`
- `TestDefaults_UncollaredOpen`
- `TestEvaluate_GrantWidensDefault`
- `TestEvaluate_AdminImpliesReadWrite`
- `TestEvaluate_WriteDoesNotImplyRead` — the case that motivated the whole lattice.
- `TestEvaluate_OrgAdminAlwaysAdmin`
- `TestEvaluate_EveryoneGrantApplies`

**Unit — provenance (`internal/acl`)**

The surfaces are only as honest as `Decision.Why`, so it gets pinned as
hard as `Decision.Perm`.

- `TestEvaluate_ReportsDefaultAsReason` — access with no row behind it still explains itself.
- `TestEvaluate_ReportsGrantAsReason` — carries the granting row, so the UI can render "by alice, 2026-08-14".
- `TestEvaluate_ReportsHandleOwnerAsReason`
- `TestEvaluate_ReportsOrgRoleAsReason`
- `TestEvaluate_GrantOverDefaultReportsGrant` — when both would grant it, the more specific reason wins, so revoking the row visibly changes the answer.
- `TestEvaluate_EveryPermittedCapabilityHasAReason` — property test: no capability is ever granted without provenance.
- `TestEvaluate_DeniedCapabilityExplainsWhy` — the `✗` rows in `whoami` need a reason too.

**Unit — visibility rule (`internal/acl`)**
- `TestCanSeeRoster_HolderOfAnyPerm` — read-only, write-only and admin principals all qualify.
- `TestCanSeeRoster_NoAccessDenied` — a member with nothing on the pipe is refused.
- `TestCanSeeRoster_OrgAdminAlways`
- `TestCanSeeRoster_WriteOnlyPrincipalQualifies` — the inbox sender case; write-only must not be mistaken for no-access.

**Unit — rendering (`internal/acl`)**
- `TestRenderPipeRoster_ColumnsAndProvenance`
- `TestRenderPipeRoster_OmitsPrincipalsWithNoAccess` — noise control.
- `TestRenderPrincipalGrants_MixesDefaultsAndGrants` — the by-principal view must show both, or it reads as "this agent can reach nothing".
- `TestRenderWhoami_ExplainsDenial`
- `TestRenderWhoami_PrintsRemediationCommand` — exact `ppz pipe acl grant …` line.
- `TestRenderWhoami_ListsWhoCanRunIt` — handle owner and org owner/admin.
- `TestRenderWhoami_NoRemediationWhenAlreadyPermitted`
- `TestRender_JSONShapeStable` — `--json` for all three; agents parse this.

**Integration**
- `TestGrant_RequiresAdminOnTarget` — a principal with `write` cannot grant.
- `TestGrant_HandleOwnerCanGrantOnOwnPipes`
- `TestGrant_BumpsACLGeneration`
- `TestRevoke_Idempotent`
- `TestRosterEndpoint_ForbiddenWithoutAccess` — HTTP surface honours the same rule as the CLI.
- `TestAccessTab_RendersStoredGrantsOnly` — the org tab must not attempt the O(principals × pipes) expansion.

**E2E**
- `tests/acl/acl-grant-and-ls-shows-row`
- `tests/acl/acl-whoami-explains-default` — a pipe nobody has granted anything on still explains why access exists.
- `tests/acl/acl-whoami-explains-denial-with-remediation`
- `tests/acl/acl-roster-visible-to-holder`
- `tests/acl/acl-roster-forbidden-to-outsider`
- `tests/acl/acl-ls-principal-shows-defaults`
- `tests/acl/ls-marks-unreadable-pipes`

### Phase 2 effort: ~3 days.

### Phase 2 implementation notes (landed)

- `internal/acl` is pure — no DB, no NATS, no HTTP. The storage layer
  hands it grants, the surfaces consume its provenance.
- The daemon is a **pure passthrough** for ACL verbs: one IPC verb
  (`IPCACL`) with an Action field rather than five, because evaluation
  happens server-side and keeping the model in one place is what stops
  the CLI and GUI drifting into two answers.
- Remediation runners are deduped by principal. The handle owner is very
  often also the org owner, and listing the same person twice under two
  labels is noise in exactly the place an agent is trying to work out
  who to ask.
- Still open from the phase 2 plan: the GUI `access` tab and the pipe-page
  ACL panel. The evaluator and renderers they need are in place and
  tested; only the templates and handlers are outstanding.

---

## Phase 3 — Enforcement

### Why the split is free

`read` and `write` are independent in NATS terms, which is why the
lattice costs nothing to enforce:

- **Writes** are a bare subject publish; the PubAck returns on the
  caller's own inbox. `publishWithAck` calls `js.Publish(ctx, subject, data)`
  with no stream lookup anywhere in the path (`internal/daemon/publish.go`).
- **Reads** go entirely through the JetStream API — `js.Stream` →
  `STREAM.INFO.<stream>`, then `CreateOrUpdateConsumer` / `OrderedConsumer`
  → `$JS.API.CONSUMER.*` (`internal/daemon/read.go`).

Disjoint permission sets. Grant `pub` on the subject and withhold every
`$JS.API.CONSUMER.*` and `STREAM.MSG.GET` for that stream, and the
principal can send and provably cannot read back.

### Credential compilation

`/auth/exchange` stops minting `pub: [">"], sub: [">"]`
(`internal/server/handlers_api.go`) and compiles the caller's effective
access instead. For principal P in account A, with
`R = {π : read ∈ eff(P,π)}` and `W = {π : write ∈ eff(P,π)}`:

```
pub.allow
  <A>.<path(π)>                              ∀ π ∈ W
  $JS.API.STREAM.INFO.<stream(π)>            ∀ π ∈ R
  $JS.API.STREAM.MSG.GET.<stream(π)>         ∀ π ∈ R
  $JS.API.DIRECT.GET.<stream(π)>             ∀ π ∈ R
  $JS.API.CONSUMER.CREATE.<stream(π)>.>      ∀ π ∈ R
  $JS.API.CONSUMER.MSG.NEXT.<stream(π)>.>    ∀ π ∈ R

sub.allow
  <A>.<path(π)>                              ∀ π ∈ R      (live tail)
  <A>._presence.>                                          (heartbeats)
  <A>._system.>                                            (invalidation)
  _INBOX.>

pub.deny
  $JS.API.STREAM.LIST, $JS.API.STREAM.NAMES               (enumeration)
  $JS.API.STREAM.CREATE.>, .UPDATE.>, .DELETE.>, .PURGE.>
```

Denying stream create/update/delete/purge outright also closes the
JS-API control-plane hole flagged in `docs/AUTH-V2.md` §Phase 3.5 — the
CLI already routes those through HTTP (`POST /api/v1/pipes`, `DELETE
/api/v1/pipes`), so nothing legitimate regresses.

### The scaling ceiling — state it, measure it

Subject-side entries collapse to wildcards (`<A>.alice.>`). **JetStream
API entries cannot.** The stream name is a single token with dots
flattened to underscores (`pipe_a1b2c3d4_alice_stdout`,
`natsubj.BuildStreamName`), and NATS wildcards match whole tokens only —
there is no pattern meaning "every heartbeat stream".

So "everyone can read heartbeats" compiles to either one allow per
heartbeat stream, or a broad allow plus one deny per stream the principal
cannot read. The compiler picks whichever list is smaller per principal.

Either way the credential grows with the number of pty sources in the
account — roughly 4 entries per shared terminal. Fine at 5 agents,
~800 entries at 200. **This is the scaling limit of the whole approach.**
It is bounded and measurable, and it is the price of enforcing at the
transport rather than proxying JetStream through the server. A benchmark
asserting credential size at 10 / 100 / 500 pipes ships with this phase
so the ceiling is a number in CI rather than a surprise in production.

### Invalidation

NATS evaluates permissions only at connect / credential load, so a
revoke does not reach a live connection on its own.

- Any grant, revoke, role change, source create or pipe create bumps `accounts.acl_generation`.
- Server publishes on `<A>._system.acl` (core, no stream, in every principal's `sub.allow`).
- Daemons re-run `/auth/exchange` on receipt and reconnect.
- Fallback: the existing 5-minute credential TTL and refresh loop (`internal/daemon/refresh.go`).

Worst case for a revoke is therefore one refresh interval, and the common
case is sub-second. The same mechanism fixes the "created a pipe, can't
use it for 5 minutes" problem that per-principal credentials would
otherwise introduce.

### HTTP control plane

Every `/api/v1` route touching a pipe gains an `eff()` check: create
requires `admin` on the parent selector (or handle ownership), destroy
and retention changes require `admin`, list filters per Phase 2's marking
rule.

### The GUI is a confused deputy

The web terminal and pipe pages read through the server's **own**
privileged NATS user (`internal/server/account_pool.go`), not the
viewer's credentials. Enforcing in the CLI and the minted credential
alone leaves the browser as an open bypass: any org member keeps seeing
your `stdout` through the web page. Every server-side read on behalf of a
session — `handlers_channel.go`, `handlers_terminal.go`, `handlers_chat.go`
— must call `eff()` for the session's user before it reads.

### Behaviour changes to announce

- **Terminal sharing becomes opt-in.** Any org member can currently watch a `ppz terminal share` session; afterwards only the handle owner can, until they grant `read` on `stdout`. Intended, but visible.
- Non-creators can no longer destroy or repurpose another principal's collared pipes.
- Existing e2e fixtures that rely on cross-member access need explicit grants.

### Phase 3 RED tests

**Unit**
- `TestCompile_ReadGrantsJSAPIOnly` — a read grant yields no subject `pub` entry.
- `TestCompile_WriteGrantsSubjectPubOnly` — a write grant yields no `$JS.API.CONSUMER.*` entry. The write-without-read proof.
- `TestCompile_DeniesStreamEnumeration`
- `TestCompile_DeniesStreamPurgeDelete`
- `TestCompile_PicksSmallerOfAllowOrDenyList`
- `BenchmarkCompile_CredentialSize` — 10 / 100 / 500 pipes.

**Integration (embedded NATS, real credentials)**
- `TestNATS_WriteOnlyPrincipal_CanPublish`
- `TestNATS_WriteOnlyPrincipal_CannotCreateConsumer` — the enforcement claim, proven against a real server.
- `TestNATS_ReadOnlyPrincipal_CannotPublish`
- `TestNATS_UngrantedPrincipal_CannotStreamInfo`
- `TestNATS_CannotEnumerateStreams`
- `TestNATS_RevokeReachesLiveConnection` — revoke, assert the `_system.acl` nudge lands and the next read fails.

**E2E**
- `tests/acl/inbox-write-allowed-read-denied` — the drop-box, end to end.
- `tests/acl/stdout-not-readable-without-grant`
- `tests/acl/stdout-readable-after-grant`
- `tests/acl/heartbeat-readable-by-everyone` — `ppz who` still works under compiled credentials.
- `tests/acl/uncollared-open-to-all-members`
- `tests/acl/gui-terminal-denies-unauthorised-viewer` — the confused-deputy regression.
- `tests/acl/revoke-takes-effect-without-restart`

### Phase 3 effort: ~4-5 days.

Roughly: 1.5 days compiler + benchmark, 1 day invalidation and the
`_system` channel, 1 day HTTP + GUI checks, 1.5 days tests and fixture
migration.

---

## Phase 4 — Key attenuation

A key may carry a **narrowing** of its principal's rights. Effective
access is `principal ∩ key scope` — intersect only, never widening. This
is where a privilege-escalation bug would live, so it ships last, alone,
with the intersection logic table-tested to exhaustion.

### Schema — `0008_key_scopes.sql`

```sql
CREATE TABLE IF NOT EXISTS key_scopes (
    key_id    uuid NOT NULL,
    key_kind  text NOT NULL CHECK (key_kind IN ('api_key','oauth_token')),
    selector  text NOT NULL,
    perm      text NOT NULL CHECK (perm IN ('read','write','admin')),
    PRIMARY KEY (key_id, key_kind, selector, perm)
);
```

No rows for a key means no attenuation — full principal rights. That
keeps every existing key working untouched.

### CLI

```
ppz svc key builder-bot --scope 'alice.inbox:write'
ppz key create --label ci-deploy --scope 'deploy.**:write' --scope 'ops.log:read'
```

### Phase 4 RED tests

**Unit**
- `TestIntersect_LiteralVsWildcard` / `_WildcardVsWildcard` / `_Disjoint` / `_MultiTokenVsSingle`
- `TestIntersect_IsCommutative` — property test over generated selector pairs.
- `TestAttenuate_NeverWidens` — property test: for random (principal, scope) pairs, the result is always a subset of the principal's rights. The escalation guard.
- `TestAttenuate_EmptyScopeMeansUnattenuated`

**Integration**
- `TestScopedKey_CannotExceedPrincipal` — scope grants `admin` on `**`, principal holds only `read` on one pipe; result is that one read.
- `TestScopedKey_RevokedPrincipalGrant_NarrowsKey`

**E2E**
- `tests/acl/scoped-key-write-only-to-one-pipe`
- `tests/acl/scoped-key-cannot-exceed-minter`

### Phase 4 effort: ~2 days.

---

## Total effort

| Phase | Days |
|---|---|
| 0 — Prerequisites | 1 |
| 1 — Principals | 2 |
| 2 — ACL store + surface | 3 |
| 3 — Enforcement | 4-5 |
| 4 — Key attenuation | 2 |
| **Total** | **12-13** |

Phases 0-2 are additive and ship without behaviour change. Phase 3 is the
breaking one and should land in a single release with the announcement
above.

## Migration

None. Prod stays wipeable, consistent with the Auth V2 phases. Existing
API keys backfill their principal from `created_by_user_id` (Phase 0);
existing pipes acquire no grant rows and fall through to the derived
defaults, which is the intended end state rather than a compatibility
shim.

## Open questions

- **Handle transfer.** Ownership is `sources.created_by_user_id`, so a
  departing human takes their handles' pipes with them. An org admin can
  always reach them (implicit `admin`), but a `ppz source transfer` verb
  is probably wanted before this matters in anger.
- **Grant expiry.** No TTL on grant rows. "Let this agent read my stdout
  for an hour" is a plausible ask and would be an additive column plus a
  sweep, but it is not in scope here.
- **Presence as core-only.** Phase 0b moves heartbeats to `_presence` but
  keeps them durable. Making them core-only is cheaper and loses nothing
  a liveness signal needs; deferred because it changes `ppz ls` output.
- **Credential size at scale.** The Phase 3 benchmark will say whether
  the ceiling is 200 agents or 2000. If it binds sooner than expected,
  the escape hatch is a NATS auth callout or per-principal sub-accounts,
  both of which are large enough to be their own plan.

## References

- `docs/AUTH-V2.md` — identity model this builds on; §"Out of scope (V3+)" Phase 3.6 is what this replaces
- `docs/WIRE.md` — subject and stream grammar
- `internal/natsubj/subjects.go` — `BuildSubject` / `BuildStreamName`, the selector grammar's anchor
- `internal/daemon/publish.go`, `internal/daemon/read.go` — why read/write split cleanly
- `internal/server/handlers_api.go` — `/auth/exchange`, where compilation lands
