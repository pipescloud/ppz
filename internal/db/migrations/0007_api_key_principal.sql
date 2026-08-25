-- 0007_api_key_principal.sql — ACL Phase 0a
--
-- An API key must resolve to a principal.
--
-- Before this migration a key carried only `created_by_user_id`, used to
-- stamp attribution on rows the key creates. The authenticated caller
-- itself had no identity: requireBearer left AuthedCaller.UserID as
-- uuid.Nil on the API-key path, so every handler wanting a user had to
-- reject API keys outright ("this endpoint requires an OAuth token").
-- Functionally a key was "full access to this org, attributed to
-- nobody" — and an ACL grant had no subject to name.
--
-- `principal_user_id` is the identity the key ACTS AS. It seeds from
-- created_by_user_id, but the two are distinct concepts and diverge in
-- ACL Phase 1: a key minted for a service account is created_by a human
-- and acts_as the service. Collapsing them would hand the service the
-- human's rights.
--
-- Idempotent — safe to re-apply on every boot.

ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS principal_user_id uuid REFERENCES users(id);

UPDATE api_keys
   SET principal_user_id = created_by_user_id
 WHERE principal_user_id IS NULL;

ALTER TABLE api_keys ALTER COLUMN principal_user_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS api_keys_principal_idx ON api_keys (principal_user_id);
