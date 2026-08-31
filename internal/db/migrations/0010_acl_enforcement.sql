-- 0010_acl_enforcement.sql — ACL Phase 3
--
-- The opt-in switch. Enforcement is per-org and defaults OFF for every
-- org, existing and new.
--
-- That default is the upgrade contract: with ACL defaults derived rather
-- than stored, flipping enforcement globally would silently make every
-- shared terminal private on upgrade morning. No org changes behaviour
-- until an admin turns this on from the Security tab, having reviewed
-- what it would break.
--
-- Idempotent — safe to re-apply on every boot.

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS acl_enforced boolean NOT NULL DEFAULT false;
