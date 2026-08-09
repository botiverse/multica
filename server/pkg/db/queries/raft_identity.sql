-- name: GetRaftIdentity :one
SELECT * FROM raft_identity
WHERE raft_server_id = $1 AND raft_sub = $2;

-- name: CreateRaftIdentity :one
INSERT INTO raft_identity (user_id, raft_server_id, raft_sub, principal_type, raft_username)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: TouchRaftIdentity :exec
UPDATE raft_identity
SET raft_username = $2,
    principal_type = $3,
    updated_at = now()
WHERE id = $1;

-- name: SyncRaftUserName :exec
UPDATE "user" SET name = $2, updated_at = now() WHERE id = $1;

-- name: GetRaftIdentityByUserID :one
-- Resolve which Raft principal a Multica user IS. Login keys identities by
-- (raft_server_id, raft_sub); this is the reverse direction, needed whenever a
-- request must answer "who is the caller, on the Raft side?" — e.g. claiming an
-- issue as yourself, where the server must resolve the assignee rather than let
-- the caller name one.
SELECT * FROM raft_identity WHERE user_id = $1;
