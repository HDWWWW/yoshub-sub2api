ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS reverse_account_priority BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN groups.reverse_account_priority IS
    'Schedule OpenAI accounts in descending priority order within this group';
