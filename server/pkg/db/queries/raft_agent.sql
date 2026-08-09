-- name: GetExternalAgentByRef :one
SELECT * FROM agent
WHERE runtime_mode = 'external'
  AND external_server_id = $1
  AND external_agent_id = $2;

-- name: CreateExternalAgent :one
-- An external agent represents a Raft agent that has joined this workspace. It
-- is executed on Raft, not by Multica: runtime_id stays NULL and external_ref
-- (external_server_id, external_agent_id) points back to the real executor.
--
-- permission_mode is set EXPLICITLY rather than left to the column default.
-- The default is 'private', which means deny-by-default in canInvokeAgent:
-- only the agent's owner may invoke or be assigned it. An external agent has
-- no owner (it belongs to a Raft principal, not a Multica user), so the
-- default made it permanently unassignable — an agent that joins a workspace
-- to take work could never be given any. It also contradicted the
-- visibility='workspace' set on the same row; migration 130 defines the pair
-- as visibility='workspace' -> permission_mode='public_to' + one workspace
-- target, and this now matches that. The caller inserts the workspace target.
INSERT INTO agent (
    workspace_id, name, runtime_mode, provider,
    external_server_id, external_agent_id, status, visibility, permission_mode
) VALUES (
    $1, $2, 'external', 'raft', $3, $4, 'idle', 'workspace', 'public_to'
)
RETURNING *;

-- name: SyncRaftWorkspaceName :exec
UPDATE workspace SET name = $2, updated_at = now() WHERE id = $1;

-- name: SyncExternalAgentName :exec
UPDATE agent SET name = sqlc.arg(name), updated_at = now()
WHERE runtime_mode = 'external'
  AND external_server_id = sqlc.arg(external_server_id)
  AND external_agent_id = sqlc.arg(external_agent_id);
