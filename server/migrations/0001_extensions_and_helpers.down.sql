-- Reverse of 0001.  Extensions are intentionally NOT dropped: other databases
-- in the cluster may depend on them, and re-creating them is cheap.
DROP TYPE IF EXISTS participant_role;
DROP TYPE IF EXISTS sync_mode;
DROP TYPE IF EXISTS room_state;
DROP TYPE IF EXISTS room_visibility;
DROP TYPE IF EXISTS content_type;
DROP TYPE IF EXISTS entitlement_tier;
DROP TYPE IF EXISTS age_band;
DROP FUNCTION IF EXISTS vybe_touch_updated_at();
DROP FUNCTION IF EXISTS uuid_generate_v7();
DROP FUNCTION IF EXISTS vybe_normalize(text);
DROP FUNCTION IF EXISTS vybe_unaccent(text);
