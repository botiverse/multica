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
