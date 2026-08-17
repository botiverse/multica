-- Reverse of 267: restore the external-agent columns and widen runtime_mode.
--
-- Deliberately NOT restored:
--
--   1. agent_external_runtime_check — its non-external branch
--      (`runtime_mode <> 'external' AND runtime_id IS NOT NULL`) is the defect
--      267 exists to remove. It re-imposes the NOT NULL that migration 251
--      dropped so runtime deletion could unbind agents instead of destroying
--      them. Recreating it here would re-break unbind on every down/up cycle.
--
--   2. idx_agent_external_ref — index builds must be CONCURRENTLY and live in
--      their own single-statement migration file (repository rule), so it
--      cannot be recreated inline here. Nothing reads these columns any more.
--
-- runtime_id stays nullable in both directions.

ALTER TABLE agent ADD COLUMN IF NOT EXISTS provider TEXT;
ALTER TABLE agent ADD COLUMN IF NOT EXISTS external_server_id TEXT;
ALTER TABLE agent ADD COLUMN IF NOT EXISTS external_agent_id TEXT;

ALTER TABLE agent DROP CONSTRAINT IF EXISTS agent_runtime_mode_check;
ALTER TABLE agent ADD CONSTRAINT agent_runtime_mode_check
    CHECK (runtime_mode IN ('local', 'cloud', 'external'));
