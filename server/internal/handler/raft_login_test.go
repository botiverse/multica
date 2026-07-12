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

func TestRaftWorkspaceSlug(t *testing.T) {
	// Stable + collision-free across principals: derived from username + sub.
	s1 := raftWorkspaceSlug(raftUserInfo{PreferredUsername: "Lincan", Sub: "abc123de-f456-7890-1234-567890abcdef"})
	if s1 != "lincan-abc123de" {
		t.Errorf("got %q, want lincan-abc123de", s1)
	}
	// Same principal -> identical slug (idempotent).
	if s2 := raftWorkspaceSlug(raftUserInfo{PreferredUsername: "Lincan", Sub: "abc123de-f456-7890-1234-567890abcdef"}); s2 != s1 {
		t.Errorf("non-deterministic slug: %q != %q", s2, s1)
	}
	// Empty username falls back to "raft" but stays unique via sub.
	if s := raftWorkspaceSlug(raftUserInfo{PreferredUsername: "", Sub: "99998888-0000-0000-0000-000000000000"}); s != "raft-99998888" {
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
