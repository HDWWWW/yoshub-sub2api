-- Additive defaults preserve existing tests and remain readable by old binaries.
SET LOCAL lock_timeout = '5s';
ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS prompt TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reasoning_effort VARCHAR(16) NOT NULL DEFAULT '';
