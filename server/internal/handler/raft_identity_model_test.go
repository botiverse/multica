package handler

import (
	"context"
	"testing"
)

// A Raft principal is a PERSON on Multica: a user plus a workspace member, and
// nothing else. Multica's `agent` type is for workers Multica itself executes,
// which is why it carries a runtime requirement and an invocation allow-list.
//
// This previously also created an `agent` row for agent principals. That made a
// Raft agent exist twice (member AND agent), let the claim action pick the wrong
// identity (assignee_type=agent), and made the UI demand a runtime for something
// Multica must never run.
//
// Contract (stdrc, 2026-08-17): "raft 上的 agent 在 multica 上是像人一样的存在
// ... Multica 上的 Agent 只用来做 worker".
//
// These tests pin the identity shape. There was no coverage here before, which
// is why the duplicate identity survived until a human noticed the symptom.

// raftTestInfo builds a distinct Raft principal per test so the cases do not
// share a workspace or a user.
func raftTestInfo(sub, principalType, serverID, serverSlug, name string) raftUserInfo {
	return raftUserInfo{
		Sub:        sub,
		Type:       principalType,
		Name:       name,
		ServerID:   serverID,
		ServerSlug: serverSlug,
		ServerName: "Identity Model Test Server",
	}
}

// cleanupRaftPrincipal removes everything a login provisions, so reruns start clean.
func cleanupRaftPrincipal(t *testing.T, userID, workspaceSlug string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM member WHERE user_id = $1`, userID)
		testPool.Exec(ctx, `DELETE FROM raft_identity WHERE user_id = $1`, userID)
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, userID)
		testPool.Exec(ctx, `
			DELETE FROM agent_invocation_target
			WHERE agent_id IN (SELECT id FROM agent WHERE workspace_id IN (
				SELECT id FROM workspace WHERE slug = $1))`, workspaceSlug)
		testPool.Exec(ctx, `
			DELETE FROM agent WHERE workspace_id IN (
				SELECT id FROM workspace WHERE slug = $1)`, workspaceSlug)
		testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, workspaceSlug)
	})
}

func TestRaftAgentLoginCreatesMemberAndNoAgentRow(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	info := raftTestInfo(
		"11111111-1111-4111-8111-111111111111", "agent",
		"22222222-2222-4222-8222-222222222222", "idmodel-agent", "identity-model-worker",
	)
	slug := raftServerWorkspaceSlug(info)

	user, _, err := testHandler.findOrCreateRaftUser(ctx, info)
	if err != nil {
		t.Fatalf("findOrCreateRaftUser: %v", err)
	}
	cleanupRaftPrincipal(t, uuidToString(user.ID), slug)

	// It is a member of the server's workspace, exactly like a human.
	var memberCount int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM member m
		JOIN workspace w ON w.id = m.workspace_id
		WHERE m.user_id = $1 AND w.slug = $2`, uuidToString(user.ID), slug).Scan(&memberCount); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if memberCount != 1 {
		t.Errorf("agent principal should be a workspace member exactly once, got %d", memberCount)
	}

	// ⭐ The regression this file exists for: NO Multica agent row.
	var agentCount int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent
		WHERE workspace_id IN (SELECT id FROM workspace WHERE slug = $1)`, slug).Scan(&agentCount); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if agentCount != 0 {
		t.Errorf("a Raft agent must NOT create a Multica agent row (Multica agents are workers); got %d", agentCount)
	}

	// And therefore no invocation-target grant either.
	var targetCount int
	if err := testPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM agent_invocation_target t
		JOIN agent a ON a.id = t.agent_id
		WHERE a.workspace_id IN (SELECT id FROM workspace WHERE slug = $1)`, slug).Scan(&targetCount); err != nil {
		t.Fatalf("count invocation targets: %v", err)
	}
	if targetCount != 0 {
		t.Errorf("no invocation-target grant should exist for a Raft principal; got %d", targetCount)
	}
}

// A human and an agent must be provisioned identically. If a future change
// reintroduces a branch on principal type, this fails alongside the test above.
func TestRaftHumanAndAgentProvisionIdentically(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	shapeOf := func(info raftUserInfo) (members int, agents int) {
		t.Helper()
		slug := raftServerWorkspaceSlug(info)
		user, _, err := testHandler.findOrCreateRaftUser(ctx, info)
		if err != nil {
			t.Fatalf("findOrCreateRaftUser(%s): %v", info.Type, err)
		}
		cleanupRaftPrincipal(t, uuidToString(user.ID), slug)
		if err := testPool.QueryRow(ctx, `
			SELECT COUNT(*) FROM member m JOIN workspace w ON w.id = m.workspace_id
			WHERE m.user_id = $1 AND w.slug = $2`, uuidToString(user.ID), slug).Scan(&members); err != nil {
			t.Fatalf("count members: %v", err)
		}
		if err := testPool.QueryRow(ctx, `
			SELECT COUNT(*) FROM agent
			WHERE workspace_id IN (SELECT id FROM workspace WHERE slug = $1)`, slug).Scan(&agents); err != nil {
			t.Fatalf("count agents: %v", err)
		}
		return members, agents
	}

	humanMembers, humanAgents := shapeOf(raftTestInfo(
		"33333333-3333-4333-8333-333333333333", "human",
		"44444444-4444-4444-8444-444444444444", "idmodel-human", "identity-model-human",
	))
	agentMembers, agentAgents := shapeOf(raftTestInfo(
		"55555555-5555-4555-8555-555555555555", "agent",
		"66666666-6666-4666-8666-666666666666", "idmodel-agent2", "identity-model-agent",
	))

	if humanMembers != agentMembers || humanAgents != agentAgents {
		t.Errorf("human and agent principals must provision identically: human=(members %d, agents %d), agent=(members %d, agents %d)",
			humanMembers, humanAgents, agentMembers, agentAgents)
	}
}
