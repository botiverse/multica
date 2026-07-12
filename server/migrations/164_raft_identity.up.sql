-- raft_identity links a Raft ("Login with Raft") principal to a Multica user.
-- A Raft principal is identified by (raft_server_id, raft_sub) from the Raft
-- userinfo claims; each maps to exactly one Multica user-principal, which holds
-- the session/PAT that the Raft agent or human uses to call the Multica API.
-- principal_type mirrors the Raft userinfo `type` claim ("human" | "agent").
CREATE TABLE raft_identity (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    raft_server_id TEXT NOT NULL,
    raft_sub TEXT NOT NULL,
    principal_type TEXT NOT NULL,
    raft_username TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (raft_server_id, raft_sub)
);

CREATE INDEX idx_raft_identity_user_id ON raft_identity (user_id);
