-- 0009_acl.sql — ACL Phase 2
--
-- Per-pipe grants. Every row is an ALLOW that widens a derived default
-- (docs/ACL.md): defaults come from (collar, pipe name) and are never
-- stored, so there is no blanket "@everyone gets everything" row to
-- subtract from — which is why there are no deny rows and no
-- precedence tiers.
--
-- `selector` matches a pipe's subject path (what natsubj.BuildSubject
-- produces, minus the account prefix), with `*` for one token and `**`
-- for one or more. Those map 1:1 onto NATS `*` and `>`, keeping the
-- Phase 3 credential compiler close to mechanical.
--
-- Idempotent — safe to re-apply on every boot.

CREATE TABLE IF NOT EXISTS acl_grants (
    id            uuid PRIMARY KEY,
    account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- The grantee. May be the @everyone pseudo-principal
    -- (00000000-0000-0000-0000-000000000002, seeded in 0007).
    principal_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    selector      text NOT NULL,
    perm          text NOT NULL CHECK (perm IN ('read','write','admin')),
    granted_by    uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, principal_id, selector, perm)
);

CREATE INDEX IF NOT EXISTS acl_grants_account_idx   ON acl_grants (account_id);
CREATE INDEX IF NOT EXISTS acl_grants_principal_idx ON acl_grants (account_id, principal_id);

-- Bumped on any grant / revoke / role / membership change. Phase 3
-- reads it to invalidate minted NATS credentials: NATS evaluates
-- permissions only at connect, so a revoke would otherwise not reach a
-- live connection until its credential expired.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS acl_generation bigint NOT NULL DEFAULT 0;
