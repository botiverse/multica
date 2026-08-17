-- Raft principals do NOT get a Multica `agent` row. On Multica a Raft agent is
-- a person (user + workspace member); Multica's `agent` type is for workers
-- Multica executes itself, which is why it carries a runtime and an invocation
-- allow-list. The external-agent queries that used to live here
-- (GetExternalAgentByRef / CreateExternalAgent / SyncExternalAgentName) are gone
-- with that model. See handler/raft_login.go and raft_identity_model_test.go.

-- name: SyncRaftWorkspaceName :exec
UPDATE workspace SET name = $2, updated_at = now() WHERE id = $1;
