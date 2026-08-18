-- 0006_audit_events.sql
--
-- Append-only trail of mutations worth answering "who changed this, when,
-- and from what to what?" about. Deliberately GENERIC: pipe create/set/
-- destroy are its first three writers, but the row shape describes any
-- actor/action/target, so key revoke, member removal and source destroy
-- slot in without a schema change.
--
--   actor_user_id     — who the action is attributed to.
--   actor_api_key_id  — the key it came through, NULL for web-session
--                       actions. This matters: on the API path the server
--                       only knows the key's CREATOR, not who typed the
--                       command, so a shared org key attributes every
--                       change to whoever minted it. Recording the key
--                       lets the GUI say "via key ppz_ab12…" rather than
--                       implying a person was at a keyboard.
--   action            — dotted <noun>.<verb> ("pipe.set"). Stable wire
--                       contract; the GUI labels and filters on it.
--   target_type/target— what was acted on ("pipe", "chat.archive").
--   before/after      — jsonb state either side of the change. NULL for
--                       the missing half of a create (no before) or a
--                       destroy (no after). Storing both is what lets the
--                       tab render "msgs 5000 → 5" instead of the useless
--                       "retention changed".
--
-- ON DELETE CASCADE on account_id only: the trail must survive the user
-- and key it names, so those columns are unreferenced by design — a
-- deleted key's id stays in the log rather than nulling out the evidence.
--
-- NOTE: no retention policy. This table grows without bound; trimming it
-- is a deliberate follow-up, not an oversight.
--
-- Idempotent: re-runnable on every boot per the Migrate convention.
CREATE TABLE IF NOT EXISTS audit_events (
    id               uuid        PRIMARY KEY,
    account_id       uuid        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    actor_user_id    uuid        NOT NULL,
    actor_api_key_id uuid,
    action           text        NOT NULL,
    target_type      text        NOT NULL,
    target           text        NOT NULL,
    before           jsonb,
    after            jsonb,
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- The org audit tab reads newest-first, bounded, for one account.
CREATE INDEX IF NOT EXISTS audit_events_account_created_idx
    ON audit_events (account_id, created_at DESC);
