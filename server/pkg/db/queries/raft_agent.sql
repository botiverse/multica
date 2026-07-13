-- name: GetExternalAgentByRef :one
SELECT * FROM agent
WHERE runtime_mode = 'external'
  AND external_server_id = $1
  AND external_agent_id = $2;

-- name: CreateExternalAgent :one
-- An external agent represents a Raft agent that has joined this workspace. It
-- is executed on Raft, not by Multica: runtime_id stays NULL and external_ref
-- (external_server_id, external_agent_id) points back to the real executor.
-- All other columns take their table defaults.
INSERT INTO agent (
    workspace_id, name, runtime_mode, provider,
    external_server_id, external_agent_id, status, visibility
) VALUES (
    $1, $2, 'external', 'raft', $3, $4, 'idle', 'workspace'
)
RETURNING *;
