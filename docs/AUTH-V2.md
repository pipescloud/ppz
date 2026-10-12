# Auth V2 — strategic identity + access for ppz

Status: **planning** (2026-05-01). Plan locked before any code lands.

## Goals

1. Close the public-GUI hole at `https://pipescloud.io/`.
2. Replace ad-hoc API keys with a coherent identity model that scales
   across CLI, daemon, future JS SDK.
3. Replace the unauthenticated public NATS port with per-user, scoped
   credentials so the SG can stay open to the world without exposure.

## Non-goals (V2)

- Multi-org per user UX polish — first-org-on-signup is fine.
- 2FA / WebAuthn — single-factor GitHub OAuth is sufficient for v2.
- SAML / SCIM — irrelevant at this scale.
- Audit log of admin actions — flagged as V3.

## End-state: one identity, four surfaces

```
        [ user (mode=github | mode=internal) ]
                       │
        ┌──────────────┼──────────────┐
        │              │              │
   browser session   API key      OAuth bearer token (CLI)
   (cookie, HMAC)    (opaque,     (short-lived, refreshable
                     hashed)        via device flow)
        │              │              │
        ▼              ▼              ▼
    GUI routes      HTTP API       HTTP API
   (/dashboard,   ──────────► single bearer-token middleware ──┐
    /orgs/*)                                                   │
                                                               ▼
                                              role check (owner/member/viewer)
                                                               │
                                                               ▼
                                                       /auth/exchange
                                                               │
                                                               ▼
                                                  NATS user JWT
                                                  (NSC-signed,
                                                  scoped subjects)
                                                               │
                                                               ▼
                                                  daemon ↔ NATS :4222
                                                  (now requires JWT,
                                                  SG can be 0.0.0.0/0)
```

Single identity, four authenticated surfaces, one validation path
in the middleware.

## Decisions (locked)

| Decision | Choice |
|---|---|
| GUI session storage | HMAC-signed cookie (stateless) |
| OAuth provider | GitHub only at v2 |
| Auto-org on signup | Yes — `<github-username>` becomes their first org, they're owner |
| API keys deprecated? | No — keep for headless / CI. Both auth methods coexist permanently. |
| CLI OAuth flow | Device flow (a la `gh auth login`) — works over SSH, no callback fragility |
| NATS auth model | NSC-style decentralized JWT |
| Token format on the wire | Opaque random for API keys (`ppz_live_…`); JWT for OAuth bearer + NATS |
| Bearer-token middleware | Single path: lookup in api_keys table OR session/token table, returns `(user_id, scope)` |
| Local dev OAuth app | Separate GitHub OAuth app `ppz-local-dev` with `http://localhost:8080/auth/github/callback` |

## Build protocol

For each phase, in this order:

1. Write the failing tests (unit + integration + e2e) — **all of them, all red**.
2. **Stop. Surface the test list for review.** No implementation yet.
3. Get explicit ack on the RED test list.
4. Implement to green, commit each green increment.
5. Run full e2e suite before tagging.

Hard rule: **no implementation lands without a test that proves it
flips red → green**. Mirrors the standing TDD-checkpoints preference.

## Phase 1 — GUI auth (GitHub OAuth + sessions)

### Schema

`internal/db/migrations/0005_users_v2.sql`:
- `ALTER TABLE users ADD COLUMN github_id BIGINT UNIQUE NULL`
- `ALTER TABLE users ADD COLUMN email VARCHAR(255) NULL`
- `ALTER TABLE users ADD COLUMN avatar_url VARCHAR(500) NULL`
- (keep `mode='github'|'internal'`; `username` already exists)

### Config

- `PPZ_GITHUB_CLIENT_ID` — Pulumi config (production), env var (local dev)
- `PPZ_GITHUB_CLIENT_SECRET` — AWS Secrets Manager (prod), `.env.local` (dev)
- `PPZ_SESSION_KEY` — 32 random bytes, AWS Secrets Manager (prod), `.env.local` (dev)
- `PPZ_BASE_URL` — `https://pipescloud.io` or `http://localhost:8080`
- `PPZ_GITHUB_AUTHORIZE_URL` — defaults to `https://github.com/login/oauth/authorize`
- `PPZ_GITHUB_TOKEN_URL` — defaults to `https://github.com/login/oauth/access_token`
- `PPZ_GITHUB_USER_URL` — defaults to `https://api.github.com/user`
- `PPZ_DEV_LOGIN` — `true` enables the test-only `POST /dev/login?user=<seed-user>`
  endpoint that mints a session for an existing internal user. **Off in production.**

The three `*_URL` overrides are how e2e tests point ppz-server at the
mock-github container instead of real GitHub. In prod they're absent
and the defaults take effect.

### Routes

| Method · Path | Behaviour |
|---|---|
| `GET /login` | Login page; "Continue with GitHub" button |
| `GET /auth/github/start` | Generate `state` (CSRF), redirect to `https://github.com/login/oauth/authorize?...` |
| `GET /auth/github/callback?code=…&state=…` | Validate state, exchange code for token, fetch user, upsert in DB, issue session cookie, redirect to `/dashboard` |
| `POST /auth/logout` | Clear session cookie, redirect to `/` |
| `GET /me` | Protected; returns the current authed user as JSON (used by tests + future GUI) |

### Middleware

- New `requireSession()` middleware applied to `/dashboard`, `/orgs/*`, `/me`.
- Reads cookie, verifies HMAC, loads user.
- If invalid/missing → 302 to `/login?next=<original-path>` (GUI) or 401 (`Accept: application/json`).
- The `next` param round-trips through the OAuth `state` blob so the
  callback can restore it. Default: `/dashboard`.

### Landing-page interaction (no change needed)

The "Pipes Cloud →" CTA on `/` still points at `/dashboard`. The
middleware does the right thing automatically:

- Authed user clicks → `/dashboard` renders directly.
- Anonymous user clicks → 302 to `/login?next=/dashboard` → GitHub flow → cookie → 302 back to `/dashboard`.

### Org gates

Every route under `/orgs/<id>` is registered through `orgSession`
(session + org membership; non-members get 404 so org existence doesn't
leak). `TestOrgRoutesUseOrgSession` fails if a route skips it. Finer
gates on top:

- `POST /orgs/<id>/keys` (mint key) — any member.
- `POST /orgs/<id>/keys/<kid>/revoke` — owner/admin for any of the org's
  keys; a member for keys they created.
- `POST /orgs/<id>/members` (add) — owner/admin.
- `POST /orgs/<id>/members/<uid>/remove` — owner/admin.
- `POST /orgs/<id>/members/<uid>/role`, invites — owner.
- `POST /orgs/<id>/transfer` — owner. **(stubbed in V1, wire to actual transfer logic here)**

Role check looks up `organisation_members.role` for the authed user
in the target org. If not-a-member → 404. If member-but-not-owner →
403.

### Phase 1 RED test list

#### Unit tests (`internal/server/auth_test.go` and friends)

- `TestSession_HMACRoundTrip` — sign + verify a session cookie cleanly.
- `TestSession_TamperedCookie_Rejected` — flipping a byte in the payload fails verify.
- `TestSession_ExpiredCookie_Rejected` — `expires_at` in the past = 401.
- `TestOAuthState_CSRFTokenRoundTrip` — generated state validates; second use rejected.
- `TestUpsertUser_GitHubID_NewRow` — `users.github_id` resolves to a fresh row on first call.
- `TestUpsertUser_GitHubID_ExistingRow` — second call returns same row.
- `TestRoleCheck_OwnerCanRevoke` / `TestRoleCheck_MemberCannotRevoke` / `TestRoleCheck_NonMemberGets404`.

#### Integration tests (`internal/server/oauth_test.go`)

Spin up `httptest.Server` as a fake GitHub. Override the token + user
endpoints in test config.

- `TestOAuth_FullFlow_NewUser` — `/login` → state → mock-GitHub → callback → cookie issued → `/me` returns 200.
- `TestOAuth_FullFlow_ReturningUser` — same, but user already exists; row updated, no duplicate.
- `TestOAuth_StateCSRF_Rejected` — callback with mismatched state → 400.
- `TestOAuth_AutoCreateOrg_OnSignup` — first signup creates an org named `<github-username>` with the user as owner.
- `TestProtectedRoute_NoSession_RedirectsToLogin` — GET `/dashboard` without cookie → 302 `/login`.

#### E2E tests (`tests/server-gui/auth-*`)

A new `mock-github` service joins the compose stack — a tiny Go
binary at `cmd/mock-github` that:

- `GET  /login/oauth/authorize?client_id=…&redirect_uri=…&state=…`
   → 302 to `<redirect_uri>?code=test_code&state=<state>`
- `POST /login/oauth/access_token` (any client_id+secret+code)
   → `{"access_token":"test_token","token_type":"bearer"}`
- `GET  /user` (any Bearer token)
   → `{"id":99001,"login":"gh-test-user","email":"ghtest@example.com","avatar_url":"…"}`

ppz-server's three `PPZ_GITHUB_*_URL` env vars point at it inside
the compose network. In production they're unset → real GitHub.

E2E tests:

- `auth-login-page-renders` — `/login` returns a "Continue with GitHub" link.
- `auth-protected-redirects-when-anon` — `/dashboard`, `/orgs/X/pipes` redirect to `/login?next=…`.
- `auth-callback-creates-user-and-org` — full flow with mock-github creates the user, auto-creates `<gh-username>` org, sets cookie, redirects to `/dashboard`.
- `auth-callback-returning-user` — second login: same `users.id`, no duplicate row.
- `auth-state-csrf-rejected` — bogus state → 400; replayed state → 400.
- `auth-revoke-key-non-owner-403` — bar (member of alpha) gets 403.
- `auth-revoke-key-owner-200` — foo (owner of alpha) succeeds.
- `auth-logout-clears-session` — POST `/auth/logout` clears cookie, dashboard back to redirect.

### Phase 1 effort: ~2-3 days.

---

## Phase 2 — CLI device flow + bearer-token middleware

### Goal

Replace `ppz daemon login --api-key <key>` (current — paste a key from
the GUI) with `ppz daemon login` (new — opens a browser flow). API
key path remains for CI / `.env` use cases.

### Schema

`internal/db/migrations/0006_oauth_tokens.sql`:
- `oauth_device_codes(device_code, user_code, user_id NULL, expires_at, polled_at)`
- `oauth_tokens(token_id, user_id, expires_at, refresh_token, revoked_at)`

### Routes

| Method · Path | Behaviour |
|---|---|
| `POST /oauth/device/code` | Issue a `(device_code, user_code, verification_uri, interval, expires_in)` pair. CLI starts polling. |
| `GET  /oauth/device/verify?user_code=…` | GUI; signed-in user confirms "yes, this CLI session is mine". |
| `POST /oauth/device/token` | CLI polls with device_code; once user_code is verified, returns bearer token. |

### Middleware (the unifying piece)

`requireAuth()`:
1. Read `Authorization: Bearer <token>`.
2. If `<token>` starts with `ppz_live_` → look up in `api_keys`, hash-compare.
3. Else → look up in `oauth_tokens`, check `expires_at`.
4. Either way: return `(user_id, org_scope, capability_scope)`.
5. Used by **all** authenticated `/api/*` routes including `/auth/exchange`.

### Phase 2 RED test list

#### Unit
- `TestDeviceCode_Generation_UniqueAndExpiring`
- `TestBearerLookup_APIKey_Hit`
- `TestBearerLookup_APIKey_HashMismatch`
- `TestBearerLookup_OAuthToken_Hit`
- `TestBearerLookup_ExpiredToken_Rejected`
- `TestBearerLookup_UnknownToken_401`

#### Integration
- `TestDeviceFlow_FullCycle` — POST `/oauth/device/code` → user verifies via session-authed GET `/oauth/device/verify` → CLI poll returns token.
- `TestDeviceFlow_PollBeforeVerify_AuthorizationPending` — RFC 8628 `authorization_pending` response.
- `TestDeviceFlow_ExpiredCode_Expired` — RFC 8628 `expired_token` after TTL.

#### E2E
- `cli-auth-login-via-device-flow` — bash test script drives `ppz daemon login`, GUI session pre-authed via cookie, asserts CLI ends up with a working token in `~/.config/ppz/credentials`.
- `cli-auth-with-api-key-still-works` — paste a key, daemon authenticates against the API. (Regression for the current path.)
- `api-bearer-401-without-auth` — `curl /api/orgs/X/pipes` without a bearer → 401.

### Phase 2 effort: ~1-2 days.

---

## Phase 3 — NATS auth (NSC decentralized JWT)

### Setup (one-time bootstrap, then env-var-driven)

A small `cmd/ppz-natskeys` bootstrap binary generates three nkey/JWT
artifacts in one shot using `github.com/nats-io/nkeys` +
`github.com/nats-io/jwt/v2`. No `nsc` CLI required on the operator's
laptop — the bootstrap is reproducible and doesn't depend on local
tooling state.

Output (printed to stdout, copy/paste into the secret store):

| Env var | What it is | Used at |
|---|---|---|
| `PPZ_NATS_OPERATOR_SEED` | Operator private nkey seed | Bootstrap only — needed when minting a new Account JWT or rotating |
| `PPZ_NATS_OPERATOR_JWT` | Operator JWT (self-signed) | Public; pairs with the seed for ergonomics |
| `PPZ_NATS_ACCOUNT_SEED` | Account private nkey seed | Bootstrap only — needed when adding/revoking signing keys or rotating account permissions |
| `PPZ_NATS_ACCOUNT_JWT` | Account JWT (signed by Operator) | NATS server boot — declares the Account + permissions |
| `PPZ_NATS_ACCOUNT_SIGNING_SEED` | Account signing nkey seed | **Hot path** — ppz-server reads at startup, mints User JWTs on `/auth/exchange` |

**Storage**:
- **Local dev**: all five values in `.env.local` (gitignored), loaded by `make dev`.
- **Prod**: all five via `pulumi config set --secret`, injected as env vars on EC2 boot — same trust boundary as `PPZ_SESSION_KEY` and `PPZ_GITHUB_CLIENT_SECRET` today.

We deliberately *don't* split the cold/hot seeds across different
stores. The `*_OPERATOR_*` and `PPZ_NATS_ACCOUNT_SEED` entries are
"cold" only in the sense that the running server never reads them —
nothing prevents future hardening (move them to a password manager,
or to a separate IAM-scoped secret) once the operational shape is
proven.

**Embedded NATS** is configured at boot with `server.Options{
TrustedOperators: [operatorJWT], AccountResolver: MEMORY, ... }` and
the Account JWT preloaded into the resolver. After boot, the server
mints User JWTs in-process using the Account signing seed — no
runtime fetch from Secrets Manager.

### `/auth/exchange` evolution

Currently returns: `{api_key, nats_url, nats_token}`.

After Phase 3:
```json
{
  "api_key":   "ppz_live_…",          // unchanged
  "nats_url":  "nats://pipescloud.io:4222",
  "nats_jwt":  "eyJ…",                 // NEW: User JWT, scoped subjects
  "nats_seed": "SU…",                  // NEW: ephemeral signing seed
  "expires_at": 1735689600
}
```

Daemon constructs NATS connection options with the JWT + seed.
NATS validates the JWT against the Account public key locally — no
callback to ppz-server.

### Subject scope per user

Given a user with org memberships in `["alpha", "beta"]`:

```yaml
publish:
  - "alpha.<own-handle>.>"   # they can publish to their own subjects
  - "beta.<own-handle>.>"
subscribe:
  - "alpha.>"                # they can subscribe to anything in their orgs
  - "beta.>"
deny:
  - "_INBOX.>"
  - "$JS.>"
```

### Phase 3 RED test list

#### Unit
- `TestNATSJWT_MintForUser_ContainsExpectedSubjects`
- `TestNATSJWT_DenyOtherOrg` — minted JWT cannot publish to a foreign org's subjects.
- `TestAccountKey_LoadFromEnv` — `PPZ_NATS_ACCOUNT_SIGNING_SEED` parses cleanly; missing/malformed values fail fast at boot.

#### Integration
- `TestEmbeddedNATS_RejectsTokenAuth` — old token-auth daemons get `Authorization Violation`.
- `TestEmbeddedNATS_AcceptsValidJWT`
- `TestEmbeddedNATS_DenyForeignOrgPublish`

#### E2E
- `nats-auth-with-jwt-publish-allowed`
- `nats-auth-with-jwt-publish-foreign-org-denied`
- `nats-auth-without-credentials-rejected`
- `nats-auth-after-key-revoke-immediate-disconnect` (acceptable to defer if too complex)

### Once Phase 3 lands

- SG `:4222` stays at `0.0.0.0/0` (auth is enforced at the NATS layer).
- Update `docs/DEPLOYMENT.md` to reflect NATS-JWT as the chosen "Option 1.5".
- Migration plan: none. Prod can be wiped at this stage; daemons
  upgraded to the new wire shape will re-login and pick up the new
  credentials on first `/auth/exchange`.

### Phase 3 effort: ~2-3 days.

---

## Phase 3.5 — Per-org NATS accounts + multi-org users + daemon JWT refresh

### Why

Phase 3's subject-based isolation closes the data-plane leak (a user
cannot read or forge another tenant's broadcasts) but leaves the JS
API control plane shared: any user JWT in the single `ppz-tenants`
account has `pub: $JS.API.>` and can call `STREAM.PURGE` on any
stream name in the system. Empirically validated against prod —
`request '$JS.API.STREAM.PURGE.source_<other-org>_…'` returns a
404 because the target stream doesn't exist *yet*, not because auth
denies it. The moment a second tenant exists, this is exploitable.

This phase also folds in the daemon's JWT refresh loop (Step 6b
deferred from Phase 3) — without it, daemons silently break when
their 5-minute JWT expires and the connection drops for any reason.

### Architecture target

```
Operator
├── Account: tenant-acme       ← per-org account, stream namespace isolated
│     signing-key: kp_acme       (alice's user JWT lives here as owner)
└── Account: tenant-globex     ← per-org account
      signing-key: kp_globex     (alice ALSO has a JWT here as viewer)
```

A user JWT is bound to one account. Multi-org alice has *two* JWTs
(one per (user, org) pair); the daemon caches both, picks one based
on the session's "current org", and reconnects to NATS when she
switches.

### Operator key: hot at runtime (changes from Phase 3)

Phase 3 kept the Operator seed cold (Pulumi state, never read by the
running server). Phase 3.5 mints new Account JWTs at runtime (when a
new org is created via OAuth signup or POST /orgs), which requires
signing with the Operator key in the request path. Two options:

1. **Operator hot in env** — same trust boundary as the existing
   Account signing seed. Fast; supports self-serve signup.
2. **Pulumi pre-mints** — every org existence requires a `pulumi up`
   round-trip. Keeps Operator cold but kills self-serve.

Going with (1). Adds one env var to the prod environment:
`PPZ_NATS_OPERATOR_SEED` (already generated by `infra/natsauth.go`,
just exposed to the EC2 instead of remaining stack-state-only).

### Account lifecycle

- **Org created** (POST /orgs or OAuth signup auto-create):
  ppz-server generates per-org Account keypair + Account signing
  keypair, builds Account claims signed by Operator, stores
  Account JWT + signing seed in `organisations.nats_account_jwt`
  and `nats_account_signing_seed` (new columns), publishes the
  Account JWT to the in-memory resolver via `nats.go`'s account
  push API.

- **Org renamed**: no NATS work; only the org's display name changes,
  the Account ID (= public key) is stable.

- **Org deleted**: the deletion flow in DB cascades, but ppz-server
  also needs to (a) delete every JS stream in that account using
  the org's own server-user JWT, (b) remove the Account JWT from
  the resolver. Without (b) connections under that account would
  remain valid for as long as their user JWTs were unexpired.

### Per-org "Server User"

Today ppz-server mints a single Server User JWT in the shared
account. After 3.5 it mints one Server User JWT *per account*,
lazily on first use, cached in process memory. Stream creation
for org acme uses acme's Server User connection.

Connection pool: `map[orgID]*nats.Conn`, lazy-init, evicted on org
delete. Each conn authed by its own Server User JWT minted from
that account's signing key.

### Subject grammar simplification

Today: `<orgID>.<handle>.<pipe>` (the orgID prefix carried tenant
isolation). After 3.5 the orgID is implied by the account, so the
prefix is redundant.

Cleaner: just `<handle>.<pipe>` per account. Stream name becomes
`source_<handle>_<pipe>` (no orgID embedded). Two streams in two
different orgs can both be named `chat.broadcast` — they're in
separate accounts, no collision.

This is a wire-shape change. Acceptable because we're wiping prod
anyway as part of 3.5's deploy, and there are no users yet.

### `/auth/exchange` accepts an explicit `org_id`

```jsonc
POST /api/v1/auth/exchange
{
  "api_key": "ppz_oauth_...",
  "org_id":  "<uuid>"   // optional; defaults to user's primary org
}
```

Server:
1. Validates the bearer + identifies the user
2. Validates user is a member of the requested org (404 if not)
3. Looks up that org's Account JWT + signing seed
4. Mints a User JWT in that account, signed by the org's signing
   key, with permissions reflecting the user's role (Phase 3.6
   tightens these; for 3.5, owner = `>`, default = also `>` since
   roles aren't enforced yet)
5. Returns `nats_user_jwt` + `nats_user_seed` + `org_id` + `org_name`

If `org_id` is omitted, server picks the user's primary org (first
owned, falling back to first-member).

### Daemon credentials: per-org map

```jsonc
// ~/.ppz/credentials.json (new shape)
{
  "url":     "https://pipescloud.io",
  "api_key": "ppz_oauth_...",            // bearer, user-scoped
  "current_org_id": "<uuid>",             // which org "ppz" defaults to
  "by_org": {
    "<acme-uuid>":   {
      "nats_user_jwt": "...",
      "nats_user_seed": "...",
      "nats_user_jwt_exp": 1777686992,    // unix ts; refresh loop uses this
      "org_name": "acme"
    },
    "<globex-uuid>": { ... }
  }
}
```

### New CLI verbs

```
ppz orgs ls                  # list orgs the bearer is a member of
ppz orgs switch <name|uuid>  # set current_org_id; daemon re-fetches JWT
                             # for that org and reconnects NATS
ppz orgs                     # alias for ls
```

### Daemon JWT refresh loop (Step 6b from Phase 3)

A goroutine per (org, daemon) tracks each cached JWT's `exp` claim
and re-runs `/auth/exchange` at `exp - 30s`. On 401 from
`/auth/exchange` (bearer revoked), the daemon evicts that org's
cached JWT, surfaces "session expired — run `ppz login`" via
`ppz status`, and continues running for other orgs.

Atomic swap: refresh writes new (jwt, seed) under a mutex;
`nats.go`'s `nats.UserJWT(...)` callback reads them on the next
reconnect (in-flight connections survive JWT expiry — NATS only
re-validates on reconnect).

### Phase 3.5 RED test list

#### Unit
- `TestMintAccountJWT` — operator-signed account JWT decodes cleanly,
  carries the org's signing key as an authorized signer.
- `TestMintUserJWT_DifferentAccounts_DifferentSigners` — same user,
  two orgs, two JWTs with different `iss` (issuer/signing key).
- `TestRefreshTimer_FiresBeforeExp` — given a JWT with exp=now+1s,
  the refresh goroutine fires within 1s.
- `TestRefreshTimer_HandlesUnauthorized` — server returns 401 →
  refresh evicts cache, surfaces "session expired".

#### Integration
- `TestEmbeddedNATS_PerOrgAccount_DataPlaneIsolation` (already passes
  in Phase 3 form, retained as regression).
- `TestEmbeddedNATS_PerOrgAccount_JSAPIControlPlaneIsolation` — user
  JWT in account-A calls `$JS.API.STREAM.PURGE.source_b_…`; server
  returns "stream not found in account A's namespace" (NOT 404 from
  account B). Empirically distinguishes account-scoped from shared.
- `TestEmbeddedNATS_AccountDeletedMidConnection` — delete account JWT
  from resolver mid-flight; verify next NATS request from a user in
  that account fails.

#### E2E
- `nats-auth-per-org-stream-cannot-be-purged-from-other-org` — two
  daemons logged into two different orgs, daemon-A tries
  `$JS.API.STREAM.PURGE` against daemon-B's stream; verified denied.
- `multi-org-user-can-switch-and-see-different-streams` — user has 2
  org memberships; `ppz orgs switch <other>` reconnects; `ppz ls`
  shows the other org's streams.
- `daemon-jwt-refresh-survives-expiry` — log in, sleep past expiry,
  publish; succeeds (refresh ran transparently).
- `daemon-jwt-refresh-handles-revoked-bearer` — log in, server-side
  revoke bearer, wait one refresh cycle; `ppz status` reports
  "session expired".

### Migration

None. Prod gets wiped: `pulumi up` with new infra (per-org
schema, hot Operator), then any logged-in CLI re-runs `ppz login`.
No existing data to preserve — single-tenant prod with one org
(jamesmiles) and one user is the entire surface.

### Phase 3.5 effort: ~3-4 days.

Roughly: 1 day per-org account minting + Operator-hot wiring,
1 day daemon credentials map + connection swap on switch + new
CLI verbs, 1 day refresh loop + tests, 0.5 day cleanup +
deployment.

---

## Phase 4 — End-to-end encryption (server cannot decrypt)

### Why

After Phase 3.5 the server still sees every message in cleartext.
To deliver "your messages are yours" — server compromise doesn't
expose message content — we move encryption to the client. Server
stores ciphertext, distributes opaque key blobs, and has no path
to plaintext.

### Architecture target

Three things, two of them per-user, one per-org:

```
Per-user (generated locally on first `ppz` run):
  enc_priv, enc_pub      Curve25519 keypair (for org-key wrapping)
  sig_priv, sig_pub      Ed25519 keypair    (for sender attribution)

Per-org (generated locally by founder when org is created):
  org_key                32 random bytes — symmetric key messages
                          are encrypted with
```

Server stores:
- every user's `enc_pub` and `sig_pub` (uploaded after local generation)
- per-(org, member): `wrapped_org_key = NaCl-box(member.enc_pub, org_key)`
- ciphertext messages with sender attribution metadata

Server never sees:
- any private key
- any plaintext message
- `org_key` itself

### Wire shape

```jsonc
// envelope sent over NATS / stored in JetStream
{
  "v":          1,
  "ciphertext": "<base64 AES-GCM ciphertext>",
  "nonce":      "<base64 12-byte nonce>",
  "sender_id":  "<user uuid>",
  "sig":        "<base64 Ed25519 signature over (nonce || ciphertext)>",
  "key_version": 1
}
```

NATS subjects, stream names, and routing are unchanged. Only the
*payload* is opaque. Existing infra (broadcast subscriber, ls,
/healthz) keeps working — it operates on envelopes, not plaintext.

### Encryption + signing on publish

```
plaintext = "hello"
nonce     = random 12 bytes
ct        = AES-GCM(org_key, plaintext, nonce)
sig       = Ed25519-sign(sig_priv, nonce || ct)
publish { v:1, ciphertext: ct, nonce, sender_id, sig, key_version }
```

### Decryption + verification on read

```
fetched   = receive envelope
sender_pub = lookup(server, sender_id).sig_pub      // cached
verify   = Ed25519-verify(sender_pub, nonce || ct, sig)
plaintext = AES-GCM-decrypt(org_key, ct, nonce)
display  { plaintext, sender: lookup(sender_id).username }
```

Verify fail → drop + warn (forged or tampered).
Decrypt fail → drop + warn (wrong key / rotation drift).

### Member join — the bootstrap problem

A new member needs `org_key`. The server can't hand it out; an
existing member must wrap it for the new member.

1. New user signs up: their CLI generates `enc_priv/pub` and
   `sig_priv/pub` locally; uploads pubs to server.
2. Owner adds new user via `POST /orgs/<id>/members`.
3. Owner's daemon notices the new member (file-poller or push),
   fetches their `enc_pub`, computes:
   `wrapped = NaCl-box(new_member.enc_pub, owner.enc_priv, org_key)`,
   uploads `wrapped` tagged with new member's user_id.
4. New member's daemon fetches their wrapped blob on next
   `/auth/exchange`, unwraps with `enc_priv`, caches `org_key`.

Server's role: route opaque blobs. Cannot decrypt.

### Member removal + key rotation

When a user is removed, the existing `org_key` is now known to
someone who shouldn't know it. Two policies:

1. **Forward-only rotation** (default): generate `org_key_v2`. New
   messages encrypted with v2. Existing members get v2 wrapped for
   them. Removed member's wrapped blob deleted. **Old messages
   remain decryptable by the removed member** — they had v1.
2. **Re-encrypt history** (expensive, deferred): rotate then
   re-encrypt every message in JetStream + DB. Heavy operation,
   not in initial scope.

For (1): envelopes carry `key_version`; recipients keep
`{v1: bytes, v2: bytes, …}` cached and pick the right key.

### Server's reduced role

Post-Phase-4, ppz-server:

- Authenticates users (unchanged: bearer + NATS JWT)
- Authorizes operations (unchanged: HTTP RBAC, NATS account isolation)
- Stores opaque ciphertext (was: cleartext)
- Stores opaque wrapped key blobs (new)
- Distributes user public keys (new — small lookup endpoint)
- **Cannot render message content in server-rendered HTML.** The
  channel page used to embed payloads in HTML; post-Phase-4 it
  serves ciphertext + sender_id + sig to the browser, which
  decrypts.

### Web GUI: from server-rendered to local-decrypted

Two view modes at pipescloud.io:

**Locked view** (default — user has not provided keys):
- Page shows envelope metadata: sender_id, timestamp, byte size,
  "🔒 encrypted (provide your org key to decrypt)"
- Useful for "yes my pipes are receiving traffic" at-a-glance

**Unlocked view** (user has imported keys this session):
- Browser holds `enc_priv` (loaded via file picker; pasted; or
  fetched from a locally-running ppz-desktop on `localhost:909x`)
- Browser fetches own wrapped org_key, unwraps with `enc_priv` via
  WebCrypto / libsodium-js
- Decrypts each message in-place, renders plaintext + sender's
  username

Keys live in browser memory only — never localStorage, never sent
to server. Tab close = unlocked view ends. Session-scoped UX
matches the trust model: server cannot decrypt your messages, but
neither can a stale tab cache.

### CLI key lifecycle

```
ppz keys generate    # mint enc + sig keypairs locally
                     # upload pubs to server
                     # store privs in ~/.ppz/keys/{enc,sig}.priv (mode 0600)
ppz keys show        # show pub fingerprints + which orgs have wrapped blobs
ppz keys export PATH # serialize {enc_priv, sig_priv} to a file
                     # (for moving to a second device)
ppz keys import PATH # load on a new device; verify pub matches what
                     # server already has registered for this user
ppz keys rotate      # mint new keypair, prove possession of old, re-wrap
                     # all org keys for new pub, mark old pub revoked.
                     # Existing signed messages stay verifiable via
                     # revoked-pub history table.
```

`ppz login` runs `ppz keys generate` automatically if no keys exist.

### Phase 4 RED test list

#### Unit
- `TestEncryptDecrypt_RoundTrip`
- `TestEncryptDecrypt_WrongKeyFails` (AES-GCM auth tag rejects)
- `TestSignVerify_RoundTrip`
- `TestSignVerify_TamperedCiphertextFails`
- `TestWrapUnwrap_OrgKey` — owner wraps for member; member unwraps;
  recovers identical org_key.

#### Integration
- `TestPublishSubscribe_E2E` — daemon-A publishes encrypted+signed;
  daemon-B (same org, has org_key) decrypts + verifies.
- `TestSubscribe_TamperedSig_DropsMessage`
- `TestSubscribe_WrongOrgKey_DropsMessage` — receiver in different
  org has different org_key → cannot decrypt → drops.

#### E2E
- `e2e-encryption-content-not-readable-on-server` — publish; dump
  the JetStream stream from ppz-server; verify only ciphertext.
- `e2e-encryption-signature-attribution` — alice publishes; bob
  reads with `from: alice` because sig verifies.
- `e2e-encryption-impersonation-rejected` — eve crafts an envelope
  claiming `sender_id: alice`, signs with eve's priv; bob drops on
  verify mismatch.
- `e2e-encryption-new-member-bootstrap` — owner adds new member,
  owner's daemon wraps + uploads, new member's daemon unwraps,
  new member can decrypt going forward.
- `e2e-encryption-member-removal-forward-only-rotation` — owner
  removes bob, alice can still decrypt new messages, bob cannot
  decrypt new messages but can still decrypt old ones (acceptable
  per chosen rotation policy).

### Migration

None. Wipe prod again as part of the 4 deploy. Single-tenant state
makes this trivial — same posture as 3.5.

### Phase 4 effort: ~5-7 days, split into sub-phases

Each sub-phase ships independently:

- **4.1: CLI/daemon encryption (no GUI)** ~2 days
  CLI generates keypairs, daemon caches keys + org_key, encrypt on
  publish + decrypt on read in the CLI. ppz-desktop similar
  treatment. Web GUI in "locked view" placeholder.

- **4.2: Member key wrapping** ~1 day
  POST /orgs/.../members triggers existing-owner wrap flow;
  new-member daemon picks up wrapped blob via /auth/exchange.

- **4.3: Browser-side decrypt in web GUI** ~2 days
  WebCrypto-based decryption for the channel-page render. Key
  import affordance (file picker; paste; QR-from-desktop is
  follow-up).

- **4.4: Key rotation + revocation** ~1-2 days
  Forward-only rotation on member-removal. Revoked-pub history
  so old messages remain verifiable.

After 4.1 the CLI-only flow is fully encrypted; the GUI is broken
until 4.3. Either deliver 4.1+4.3 in one cycle or accept temporary
GUI degradation.

### Open questions

- **Browser key import UX.** File picker is functional but ugly;
  paste-into-form leaks via clipboard managers. The cleanest is a
  WebSocket from the browser tab to a locally-running ppz-desktop
  on `127.0.0.1:909x`, requesting "I'm pipescloud.io's tab —
  send me the keys for org X". User confirms in desktop, keys
  flow to browser memory. Adds desktop dependency for full GUI;
  worth it.

- **WebCrypto vs libsodium-js.** WebCrypto handles AES-GCM and
  Ed25519 natively. NaCl-box (Curve25519 + XSalsa20+Poly1305) is
  not in WebCrypto and requires libsodium-js (~200KB gzipped). If
  we want zero JS deps for the GUI we'd switch wrapping to
  WebCrypto's ECIES-equivalent (HKDF + AES-GCM with ECDH derived
  shared secret). Slightly more code in CLI but avoids the JS
  payload. Decision: use libsodium-js for symmetry between CLI
  and browser; the GUI is already a multi-KB page.

- **Key recovery.** If a user loses their `enc_priv`, they lose
  access to every org they're a member of (their wrapped blobs
  become unreadable). Owners can re-add them with fresh keys —
  but that's the same as a new-member bootstrap. Worth a
  documented "if you lose your keys, ask an org owner to re-invite
  you" path. Server-side key escrow defeats the trust model and
  is rejected.

---

## Out of scope (V3+)

- Audit log table + UI of admin actions.
- 2FA / passkeys.
- Federated identity (Google, Microsoft, SAML).
- Phase 3.6: per-user role-scoped JWTs + HTTP RBAC middleware
  (`organisation_members.role`, `requireRole(...)`). Lifts user
  permissions from default-`>` to owner/member/viewer/bot
  patterns. Lands after 3.5; explicitly *not* in 3.5 scope so
  the per-org-account work isn't gated on the role design.
- Multi-org GUI navigation (org switcher dropdown). The HTTP
  routes are already org-scoped via URL; a GUI affordance is
  pure UX work, fits anywhere after 3.5.
- Token rotation policies, key cycling automation.
- Re-encrypting historical messages on member removal (Phase 4
  ships forward-only rotation; full re-encryption deferred).
- HSM-backed key storage for owner enc_priv (extra hardening if a
  laptop compromise is in the threat model).

## References

- RFC 8628 — OAuth 2.0 Device Authorization Grant
- NATS NSC docs — https://docs.nats.io/using-nats/nats-tools/nsc
- Existing V1 schema — `internal/db/migrations/0004_users.sql`
- `docs/DEPLOYMENT.md` — § NATS exposure (Option 1)
