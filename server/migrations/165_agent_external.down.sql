DROP INDEX IF EXISTS idx_agent_external_ref;
ALTER TABLE agent DROP CONSTRAINT IF EXISTS agent_external_runtime_check;
ALTER TABLE agent DROP COLUMN IF EXISTS external_agent_id;
ALTER TABLE agent DROP COLUMN IF EXISTS external_server_id;
ALTER TABLE agent DROP COLUMN IF EXISTS provider;
ALTER TABLE agent DROP CONSTRAINT agent_runtime_mode_check;
ALTER TABLE agent ADD CONSTRAINT agent_runtime_mode_check
    CHECK (runtime_mode IN ('local', 'cloud'));
ALTER TABLE agent ALTER COLUMN runtime_id SET NOT NULL;
