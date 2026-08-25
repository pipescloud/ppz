-- 0008_principals.sql — ACL Phase 1
--
-- Two additions the ACL model needs a subject for:
--
--   1. An org `admin` tier. Today "owner" means users.id ==
--      accounts.owner_user_id and everyone else in account_members is a
--      member — account_members has no role column at all, so the ACL
--      rule "org owner and admin hold implicit admin on **" has nothing
--      to read.
--
--   2. Service accounts. An agent needs its own identity, distinct from
--      the human who spawned it: a real principal that holds grants,
--      owns handles, and is attributed on `ppz who` and on every message
--      it publishes.
--
-- Idempotent — safe to re-apply on every boot.

-- ─── Org roles ────────────────────────────────────────────────────────
--
-- accounts.owner_user_id stays the authority for 'owner'; this column
-- distinguishes admin from member. Both are read by RoleInOrg.

ALTER TABLE account_members ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'member';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_name = 'account_members' AND constraint_name = 'account_members_role_check'
    ) THEN
        ALTER TABLE account_members
            ADD CONSTRAINT account_members_role_check
            CHECK (role IN ('owner','admin','member'));
    END IF;
END $$;

-- InsertAccount does not add a member row for the owner, so the members
-- table can be missing the one person who definitely belongs. Backfill
-- it, both so the GUI can render roles and so role reads have a row.
INSERT INTO account_members (account_id, user_id, role)
SELECT id, owner_user_id, 'owner' FROM accounts WHERE owner_user_id IS NOT NULL
    ON CONFLICT (account_id, user_id) DO UPDATE SET role = 'owner';

-- ─── Service accounts ─────────────────────────────────────────────────

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.check_constraints
        WHERE constraint_name = 'users_mode_check'
    ) THEN
        ALTER TABLE users DROP CONSTRAINT users_mode_check;
    END IF;
END $$;

ALTER TABLE users ADD COLUMN IF NOT EXISTS service_account_id uuid
    REFERENCES accounts(id) ON DELETE CASCADE;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_name = 'users' AND constraint_name = 'users_mode_check'
    ) THEN
        ALTER TABLE users
            ADD CONSTRAINT users_mode_check
            CHECK (mode IN ('github','internal','service'));
    END IF;
    -- A service row must belong to an account; a human row must not.
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_name = 'users' AND constraint_name = 'users_service_owner_check'
    ) THEN
        ALTER TABLE users
            ADD CONSTRAINT users_service_owner_check
            CHECK ((mode = 'service') = (service_account_id IS NOT NULL));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS users_service_account_idx ON users (service_account_id);

-- ─── @everyone ────────────────────────────────────────────────────────
--
-- The pseudo-principal an ACL row names to grant the whole account.
-- Fixed UUID, seeded exactly the way the 'unauthenticated' placeholder
-- is, so the FK and the uniqueness constraint on acl_grants stay honest.
-- Must never be uuid.Nil, which means "unauthenticated" everywhere else.

INSERT INTO users (id, username, email, mode)
VALUES ('00000000-0000-0000-0000-000000000002',
        'everyone',
        'everyone@local',
        'internal')
ON CONFLICT (id) DO NOTHING;
