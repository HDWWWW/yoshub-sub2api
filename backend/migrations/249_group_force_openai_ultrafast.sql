ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS force_openai_ultrafast BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS filter_openai_service_tier BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN groups.force_openai_ultrafast IS
    'Force OpenAI requests to use service_tier=ultrafast; mutually exclusive with force_openai_fast';
COMMENT ON COLUMN groups.free_openai_fast IS
    'Bill Fast and Ultrafast requests at the equivalent Standard price';

ALTER TABLE groups ADD CONSTRAINT groups_openai_fast_mode_exclusive
    CHECK (force_openai_fast::integer + force_openai_ultrafast::integer + filter_openai_service_tier::integer <= 1);

COMMENT ON COLUMN groups.filter_openai_service_tier IS
    'Remove OpenAI service_tier while preserving global block rules';
