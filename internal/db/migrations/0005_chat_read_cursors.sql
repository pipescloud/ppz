-- 0005_chat_read_cursors.sql
--
-- Per-user, per-window read position for the web chat console's unread badges.
-- A conversation is (kind, acting, target):
--   kind    — "source" (a DM) or "pipe" (a shared room).
--   acting  — the handle the viewer is acting AS (their identity). For a DM the
--             read position is per (my handle ↔ counterparty), because the
--             counterparty's messages to me land in MY inbox, which differs by
--             identity. "" for pipes (shared) and the god's-eye fallback.
--   target  — the source handle (DM counterparty) or dotted pipe path.
--
-- last_read_seq is the sequence in the relevant stream the viewer has read up
-- to (their own inbox for a DM, the pipe's stream for a pipe). A cursor only
-- ever advances (writers use GREATEST), so a late write can't rewind it.
--
-- Idempotent: re-runnable on every boot per the Migrate convention.
CREATE TABLE IF NOT EXISTS chat_read_cursors (
    account_id    uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    user_id       uuid        NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    kind          text        NOT NULL,
    acting        text        NOT NULL DEFAULT '',
    target        text        NOT NULL,
    last_read_seq bigint      NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, user_id, kind, acting, target)
);
