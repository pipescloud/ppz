-- 0005_chat_read_cursors.sql
--
-- Per-user, per-window read position for the web chat console's unread badges.
-- A window is (kind, target): kind in {source, pipe}; target is the source
-- handle (agents/inboxes) or the dotted "<manifold>.<name>" pipe path. Agents
-- and inboxes are both kind=source, matching the chat window resolver.
--
-- last_read_seq is the JetStream stream sequence the user has read up to; the
-- roster computes unread as max(0, stream.LastSeq - last_read_seq). A cursor
-- only ever advances (writers use GREATEST), so a late-arriving reader can't
-- rewind someone's read position.
--
-- Idempotent: re-runnable on every boot per the Migrate convention.
CREATE TABLE IF NOT EXISTS chat_read_cursors (
    account_id    uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id       uuid        NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    kind          text        NOT NULL,
    target        text        NOT NULL,
    last_read_seq bigint      NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, user_id, kind, target)
);
