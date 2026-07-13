package handler

import "testing"

func TestSlugifyRaft(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Alice", "alice"},
		{"Ada Lovelace", "ada-lovelace"},
		{"  spaced  out  ", "spaced-out"},
		{"UPPER_Case.Mix", "upper-case-mix"},
		{"a--b__c", "a-b-c"},
		{"---trim---", "trim"},
		{"", ""},
		{"!!!", ""},
		{"café", "caf"}, // non-ascii dropped
	}
	for _, c := range cases {
		if got := slugifyRaft(c.in); got != c.want {
			t.Errorf("slugifyRaft(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRaftServerWorkspaceSlug(t *testing.T) {
	// Workspace identity comes from the Raft SERVER, so all principals from the
	// same server converge on the same slug regardless of who logs in.
	a := raftServerWorkspaceSlug(raftUserInfo{ServerSlug: "dev", ServerID: "abc123de-f456-7890-1234-567890abcdef"})
	if a != "dev-abc123de" {
		t.Errorf("got %q, want dev-abc123de", a)
	}
	// Different principals, same server -> same workspace slug.
	b := raftServerWorkspaceSlug(raftUserInfo{ServerSlug: "dev", ServerID: "abc123de-f456-7890-1234-567890abcdef", Sub: "someone-else"})
	if b != a {
		t.Errorf("same server should map to same workspace: %q != %q", b, a)
	}
	// Empty server slug falls back to "raft" but stays unique via server id.
	if s := raftServerWorkspaceSlug(raftUserInfo{ServerSlug: "", ServerID: "99998888-0000-0000-0000-000000000000"}); s != "raft-99998888" {
		t.Errorf("got %q, want raft-99998888", s)
	}
}

func TestRaftSyntheticEmailStableAndScoped(t *testing.T) {
	info := raftUserInfo{Type: "agent", Sub: "sub-1", ServerID: "srv-1"}
	a := raftSyntheticEmail(info)
	b := raftSyntheticEmail(info)
	if a != b {
		t.Errorf("synthetic email not stable: %q != %q", a, b)
	}
	if a != "agent.sub-1@srv-1.raft.invalid" {
		t.Errorf("got %q, want agent.sub-1@srv-1.raft.invalid", a)
	}
}
