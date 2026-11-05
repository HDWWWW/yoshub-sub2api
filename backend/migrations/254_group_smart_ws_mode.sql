ALTER TABLE groups ADD COLUMN IF NOT EXISTS smart_ws_mode varchar(16) NOT NULL DEFAULT 'inherit';
ALTER TABLE groups ADD CONSTRAINT groups_smart_ws_mode_check CHECK (smart_ws_mode IN ('inherit', 'enabled', 'disabled'));
