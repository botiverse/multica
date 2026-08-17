-- Migration 267: remove the external-agent schema added by 266.
--
-- WHY IT GOES AWAY. A Raft agent is a PERSON on Multica — a user plus a
-- workspace member — not a Multica agent. Multica's `agent` entity is for
-- workers Multica itself executes, which is exactly why it carries a runtime
-- and an invocation allow-list. Representing a Raft agent as one made it exist
-- twice (member AND agent), made the claim path assign the wrong identity, and
-- made the UI demand a runtime for something Multica must never run.
-- Contract (stdrc, 2026-08-17): a Raft agent on Multica is like a person;
-- Multica agents are only workers.
--
-- 🔴 WHY THIS IS URGENT, INDEPENDENT OF THE MODEL CHANGE. 266's CHECK said:
--
--     OR (runtime_mode <> 'external' AND runtime_id IS NOT NULL)
--
-- which forbids runtime_id IS NULL for every ordinary local/cloud agent. That
-- is precisely the invariant migration 251 (MUL-5559) had just REMOVED on
-- purpose, so that deleting a runtime UNBINDS its agents instead of destroying
-- them ("Retiring a laptop is an ordinary action; losing the agents configured
-- on it is not an ordinary consequence"). 266 silently reinstated the old rule
-- for every non-external row and broke unbind and runtime deletion — both
-- return 500. The external half of the constraint was fine; the half that
-- spoke for ordinary agents was not mine to assert.
--
-- ⚠️ 266's own down-migration has the same defect (`ALTER COLUMN runtime_id SET
-- NOT NULL`) and must NOT be used to reverse this: it would re-break 251.
-- runtime_id stays nullable here, permanently.

-- Rows only the retired model could create. No FKs/cascades in this schema, so
-- dependents are removed explicitly (repository rule).
DELETE FROM agent_invocation_target
WHERE agent_id IN (SELECT id FROM agent WHERE runtime_mode = 'external');

DELETE FROM agent WHERE runtime_mode = 'external';

DROP INDEX IF EXISTS idx_agent_external_ref;

ALTER TABLE agent DROP CONSTRAINT IF EXISTS agent_external_runtime_check;

ALTER TABLE agent DROP COLUMN IF EXISTS external_agent_id;
ALTER TABLE agent DROP COLUMN IF EXISTS external_server_id;
ALTER TABLE agent DROP COLUMN IF EXISTS provider;

ALTER TABLE agent DROP CONSTRAINT IF EXISTS agent_runtime_mode_check;
ALTER TABLE agent ADD CONSTRAINT agent_runtime_mode_check
    CHECK (runtime_mode IN ('local', 'cloud'));
