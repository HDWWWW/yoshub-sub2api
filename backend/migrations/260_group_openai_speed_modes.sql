-- Preserve existing group values while allowing the official Fast fallback
-- alongside Ultrafast. Filtering remains exclusive with either forced tier.
ALTER TABLE groups
    DROP CONSTRAINT IF EXISTS groups_openai_fast_mode_exclusive;

ALTER TABLE groups
    ADD CONSTRAINT groups_openai_fast_mode_exclusive
    CHECK (NOT filter_openai_service_tier OR (NOT force_openai_fast AND NOT force_openai_ultrafast));

COMMENT ON COLUMN groups.force_openai_ultrafast IS
    'Force service_tier=ultrafast for supported models; takes precedence over force_openai_fast, which can remain enabled as fallback';
COMMENT ON COLUMN groups.free_openai_fast IS
    'Bill Fast and Ultrafast requests at the equivalent Standard price';
