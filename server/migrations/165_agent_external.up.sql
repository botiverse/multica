-- External agents: a Raft agent joins Multica but is executed on the Raft side,
-- not by Multica. It is a real third runtime_mode ('external'), NOT a local/cloud
-- agent with a fabricated runtime. runtime_mode answers "who executes this
-- agent"; for local/cloud that is Multica, for a Raft agent it is Raft. Its
-- runtime_id is NULL (there is no Multica executor) and external_ref points back
-- to the real executor on Raft.
ALTER TABLE agent ALTER COLUMN runtime_id DROP NOT NULL;

ALTER TABLE agent DROP CONSTRAINT agent_runtime_mode_check;
ALTER TABLE agent ADD CONSTRAINT agent_runtime_mode_check
    CHECK (runtime_mode IN ('local', 'cloud', 'external'));

ALTER TABLE agent ADD COLUMN provider TEXT;
ALTER TABLE agent ADD COLUMN external_server_id TEXT;
ALTER TABLE agent ADD COLUMN external_agent_id TEXT;

-- Invariant: an external agent has NO Multica runtime and MUST carry an
-- external_ref; a local/cloud agent MUST have a real runtime. NOT NULL means
-- "there is a real Multica executor"; when the executor is elsewhere the honest
-- answer is NULL + a pointer, never a fabricated runtime row.
ALTER TABLE agent ADD CONSTRAINT agent_external_runtime_check CHECK (
    (runtime_mode = 'external'
        AND runtime_id IS NULL
        AND external_server_id IS NOT NULL
        AND external_agent_id IS NOT NULL)
    OR (runtime_mode <> 'external' AND runtime_id IS NOT NULL)
);

-- One Multica external-agent entity per (raft server, raft agent).
CREATE UNIQUE INDEX idx_agent_external_ref
    ON agent (external_server_id, external_agent_id)
    WHERE runtime_mode = 'external';
