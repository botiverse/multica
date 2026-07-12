package handler

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
)

// This file turns Multica into a "Raft App": it serves the agent-behavior
// manifest that Raft agents discover, and the OAuth callback that Raft's
// `integration invoke` flow calls to establish an authenticated session cookie
// before invoking the manifest's HTTP actions.

// raftAgentManifest mirrors the raft-agent-manifest.v0 schema consumed by the
// Raft CLI (packages/cli/src/commands/integration/manifest.ts).
type raftAgentManifest struct {
	Schema      string               `json:"schema"`
	Service     string               `json:"service"`
	Name        string               `json:"name"`
	Description string               `json:"description"`
	AppOrigin   string               `json:"app_origin"`
	DocsURL     string               `json:"docs_url,omitempty"`
	Execution   raftManifestExec     `json:"execution"`
	Auth        raftManifestAuth     `json:"auth"`
	Actions     []raftManifestAction `json:"actions"`
}

type raftManifestExec struct {
	Mode    string `json:"mode"`
	BaseURL string `json:"base_url,omitempty"`
}

type raftManifestAuth struct {
	Type     string `json:"type"`
	LoginURL string `json:"login_url,omitempty"`
}

type raftManifestAction struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description,omitempty"`
	Endpoint    raftManifestEndpoint         `json:"endpoint"`
	Parameters  map[string]raftManifestField `json:"parameters,omitempty"`
	Returns     map[string]raftManifestField `json:"returns,omitempty"`
}

type raftManifestEndpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type raftManifestField struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// raftAppOrigin returns the public origin where this Multica backend serves its
// /api and callback routes, used as the manifest base_url. Prefers the operator
// -set backend URL, falling back to the frontend origin for single-host dev.
func raftAppOrigin() string {
	if v := normalizePublicURL(os.Getenv("MULTICA_PUBLIC_URL")); v != "" {
		return v
	}
	return normalizePublicURL(os.Getenv("FRONTEND_ORIGIN"))
}

// RaftAgentManifest serves the agent-behavior manifest at
// /.well-known/raft-agent-manifest.json. It is public (discovery only) and
// grants no access on its own — every action still authenticates via the
// session established by RaftCallback.
//
// Actions map to Multica's existing REST API. list-workspaces is workspace
// -agnostic (it enumerates the caller's workspaces), so it works with just the
// session cookie; workspace-scoped actions additionally need a resolved
// workspace (see the X-Workspace-ID handling), added incrementally.
func (h *Handler) RaftAgentManifest(w http.ResponseWriter, r *http.Request) {
	origin := raftAppOrigin()
	manifest := raftAgentManifest{
		Schema:      "raft-agent-manifest.v0",
		Service:     "multica",
		Name:        "Multica",
		Description: "Multica task management: view workspaces, issues, and agents, and publish tasks as a Raft agent.",
		AppOrigin:   origin,
		Execution:   raftManifestExec{Mode: "http_api", BaseURL: origin},
		Auth:        raftManifestAuth{Type: "login_with_raft"},
		Actions: []raftManifestAction{
			{
				Name:        "list-workspaces",
				Description: "List the workspaces the authenticated Raft principal can access.",
				Endpoint:    raftManifestEndpoint{Method: "GET", Path: "/api/workspaces"},
				Returns: map[string]raftManifestField{
					"workspaces": {Type: "array", Description: "Workspaces the caller belongs to."},
				},
			},
		},
	}
	writeJSON(w, http.StatusOK, manifest)
}

// RaftCallback is the OAuth return endpoint registered with the Raft server. The
// Raft `integration invoke` flow calls GET <returnUrl>?code=<code> to establish
// a session: we run the shared Login-with-Raft exchange and set Multica's auth
// cookie on the response, which the caller captures and replays on subsequent
// action calls. The browser "Login with Raft" flow may also land here.
func (h *Handler) RaftCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	user, _, authErr := h.authenticateRaft(r.Context(), code, "")
	if authErr != nil {
		writeError(w, authErr.status, authErr.msg)
		return
	}

	tokenString, err := h.issueJWT(user)
	if err != nil {
		slog.Warn("raft callback: failed to issue JWT", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	if err := auth.SetAuthCookies(w, tokenString); err != nil {
		slog.Warn("raft callback: failed to set auth cookies", "error", err)
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(72 * time.Hour)) {
			http.SetCookie(w, cookie)
		}
	}

	// A browser lands here as a top-level navigation and expects to continue
	// into the app; the integration-invoke flow just needs the Set-Cookie and
	// ignores the body.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		dest := normalizePublicURL(os.Getenv("FRONTEND_ORIGIN"))
		if dest == "" {
			dest = "/"
		}
		http.Redirect(w, r, dest, http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"user": userToResponse(user),
	})
}
