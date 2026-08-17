package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
		Description: "Multica task management: view workspaces, issues, and agents; publish tasks; and take and progress your own work as a Raft agent.",
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
			{
				Name:        "list-issues",
				Description: "List issues in a workspace. Pass workspace_slug from list-workspaces.",
				Endpoint:    raftManifestEndpoint{Method: "GET", Path: "/api/issues"},
				Parameters: map[string]raftManifestField{
					"workspace_slug": {Type: "string", Description: "Target workspace slug.", Required: true},
				},
				Returns: map[string]raftManifestField{
					"issues": {Type: "array", Description: "Issues in the workspace."},
				},
			},
			{
				Name:        "list-agents",
				Description: "List the agents in a workspace. Pass workspace_slug from list-workspaces.",
				Endpoint:    raftManifestEndpoint{Method: "GET", Path: "/api/agents"},
				Parameters: map[string]raftManifestField{
					"workspace_slug": {Type: "string", Description: "Target workspace slug.", Required: true},
				},
				Returns: map[string]raftManifestField{
					"agents": {Type: "array", Description: "Agents in the workspace."},
				},
			},
			{
				Name:        "create-issue",
				Description: "Publish a task/issue into a workspace. Pass workspace_slug from list-workspaces.",
				Endpoint:    raftManifestEndpoint{Method: "POST", Path: "/api/raft/issues"},
				Parameters: map[string]raftManifestField{
					"workspace_slug": {Type: "string", Description: "Target workspace slug.", Required: true},
					"title":          {Type: "string", Description: "Issue title.", Required: true},
					"description":    {Type: "string", Description: "Issue description (optional)."},
					"priority":       {Type: "string", Description: "none|low|medium|high|urgent (optional)."},
				},
				Returns: map[string]raftManifestField{
					"identifier": {Type: "string", Description: "The created issue identifier, e.g. LIN-1."},
				},
			},
			{
				Name:        "claim-issue",
				Description: "Take an issue as your own work. The server assigns it to YOU; there is no assignee parameter, so this cannot be used to hand work to someone else.",
				Endpoint:    raftManifestEndpoint{Method: "POST", Path: "/api/raft/issues/{id}/claim"},
				Parameters: map[string]raftManifestField{
					"id":             {Type: "string", Description: "Issue id or identifier, e.g. LIN-1.", Required: true},
					"workspace_slug": {Type: "string", Description: "Target workspace slug.", Required: true},
				},
				Returns: map[string]raftManifestField{
					"issue": {Type: "object", Description: "The updated issue, now assigned to the caller."},
				},
			},
			{
				Name:        "set-issue-status",
				Description: "Move an issue you own along. Allowed when you are its assignee or its creator; changing someone else's issue is a role capability and is not exposed yet.",
				Endpoint:    raftManifestEndpoint{Method: "POST", Path: "/api/raft/issues/{id}/status"},
				Parameters: map[string]raftManifestField{
					"id":             {Type: "string", Description: "Issue id or identifier, e.g. LIN-1.", Required: true},
					"workspace_slug": {Type: "string", Description: "Target workspace slug.", Required: true},
					"status":         {Type: "string", Description: "Target status, e.g. backlog|todo|in_progress|done|canceled.", Required: true},
				},
				Returns: map[string]raftManifestField{
					"issue": {Type: "object", Description: "The updated issue."},
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
		"user": h.userToResponse(user),
	})
}

// RaftCreateIssueRequest is CreateIssueRequest plus the workspace_slug the Raft
// integration-invoke flow passes in the body (that transport forwards params in
// the JSON body, not as the X-Workspace-ID header or a query param).
type RaftCreateIssueRequest struct {
	WorkspaceSlug string `json:"workspace_slug"`
	CreateIssueRequest
}

// RaftCreateIssue lets a Raft agent publish a task into a workspace it belongs
// to, selecting the workspace by slug in the body. It resolves + authorizes the
// workspace, then delegates to CreateIssue — rewriting the request so
// CreateIssue's workspace resolver (query) and body decode both see the right
// values — to reuse all of CreateIssue's validation and creation logic.
func (h *Handler) RaftCreateIssue(w http.ResponseWriter, r *http.Request) {
	creatorID, ok := requireUserID(w, r)
	if !ok {
		return
	}

	var req RaftCreateIssueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	slug := strings.TrimSpace(req.WorkspaceSlug)
	if slug == "" {
		writeError(w, http.StatusBadRequest, "workspace_slug is required")
		return
	}

	ws, err := h.Queries.GetWorkspaceBySlug(r.Context(), slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "workspace not found")
		return
	}
	userUUID, ok := parseUUIDOrBadRequest(w, creatorID, "user_id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      userUUID,
		WorkspaceID: ws.ID,
	}); err != nil {
		writeError(w, http.StatusForbidden, "not a member of the target workspace")
		return
	}

	// Reuse CreateIssue's full logic: re-encode just the issue fields as the
	// body and put the resolved workspace on the query, where resolveWorkspaceID
	// picks it up.
	body, err := json.Marshal(req.CreateIssueRequest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	q := r.URL.Query()
	q.Set("workspace_id", uuidToString(ws.ID))
	r.URL.RawQuery = q.Encode()

	h.CreateIssue(w, r)
}

// --- Self-scoped work actions (Layer 1) ----------------------------------
//
// These let a Raft agent do its own work: take a task, move it along, talk
// about it. They deliberately do NOT let an agent act on anyone else's behalf.
//
// WHY SELF-SCOPED, and why not just expose PUT /api/issues/{id}:
// that endpoint accepts assignee_type/assignee_id, so handing it to agents
// would let any agent assign work to any other agent or member. "Who may
// manage whom" is a ROLE question, and the role gate does not exist yet — it
// is Step 3, which reads server_role through from Raft. Shipping a generic
// update action now would grant manage-others authority before the gate that
// is supposed to govern it, and it would be very hard to take back once agents
// depend on it. So: the server decides the assignee (always the caller), and
// status changes are limited to work the caller already owns.
//
// When Step 3 lands, an assign-others action can be added behind the role gate.

// raftIssueScope is the workspace_slug every Raft action carries, because the
// agent addresses workspaces by slug rather than by internal id.
type raftIssueScope struct {
	WorkspaceSlug string `json:"workspace_slug"`
}

// resolveRaftWorkspace validates the slug and the caller's membership, and
// returns the workspace. Mirrors RaftCreateIssue's preamble.
func (h *Handler) resolveRaftWorkspace(w http.ResponseWriter, r *http.Request, slug string) (db.Workspace, string, bool) {
	callerID, ok := requireUserID(w, r)
	if !ok {
		return db.Workspace{}, "", false
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		writeError(w, http.StatusBadRequest, "workspace_slug is required")
		return db.Workspace{}, "", false
	}
	ws, err := h.Queries.GetWorkspaceBySlug(r.Context(), slug)
	if err != nil {
		writeError(w, http.StatusNotFound, "workspace not found")
		return db.Workspace{}, "", false
	}
	callerUUID, ok := parseUUIDOrBadRequest(w, callerID, "user_id")
	if !ok {
		return db.Workspace{}, "", false
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
		UserID:      callerUUID,
		WorkspaceID: ws.ID,
	}); err != nil {
		writeError(w, http.StatusForbidden, "not a member of the target workspace")
		return db.Workspace{}, "", false
	}
	return ws, callerID, true
}

// callerAsAssignee answers "who is the caller, as an assignee?" — the server
// resolves this so the caller can never name someone else. A Raft agent that
// has joined this workspace is represented by an external agent row, so it
// assigns as that agent; a human Raft principal assigns as a member.
// A Raft caller is ALWAYS assigned as a member, whether it is a human or an
// agent. On Multica a Raft principal is a person; Multica's `agent` assignee
// type belongs to workers Multica executes itself.
//
// This used to resolve an agent caller to its Multica `agent` row and return
// assignee_type="agent". That was the wrong half of a duplicate identity: a
// Raft agent had both a member row and an agent row, and this function picked
// the agent one. The agent row is no longer created (see findOrCreateRaftUser).
func (h *Handler) callerAsAssignee(_ *http.Request, callerID string, _ db.Workspace) (string, string, error) {
	if _, err := util.ParseUUID(callerID); err != nil {
		return "", "", err
	}
	return "member", callerID, nil
}

// delegateIssueUpdate hands the request to UpdateIssue, which owns all the real
// update logic (events, timeline, notifications). Same delegation shape as
// RaftCreateIssue.
//
// It marshals ONLY the fields being changed, rather than the whole
// UpdateIssueRequest. Those fields are pointers WITHOUT omitempty, so encoding
// the struct emits `"assignee_type":null,"assignee_id":null` for a
// status-only update — and UpdateIssue reads an explicit null as "clear this
// field". Sending the struct therefore silently UNASSIGNED the issue on every
// status change: a worker that claimed a task lost it the moment it moved the
// task to in_progress. Caught by the second status call failing the
// assignee-or-creator check that the first call had just invalidated.
func (h *Handler) delegateIssueUpdate(w http.ResponseWriter, r *http.Request, ws db.Workspace, fields map[string]any) {
	body, err := json.Marshal(fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	q := r.URL.Query()
	q.Set("workspace_id", uuidToString(ws.ID))
	r.URL.RawQuery = q.Encode()
	h.UpdateIssue(w, r)
}

// RaftClaimIssue assigns an issue to the CALLER. There is no assignee
// parameter on purpose — see the self-scoped note above.
func (h *Handler) RaftClaimIssue(w http.ResponseWriter, r *http.Request) {
	var req raftIssueScope
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ws, callerID, ok := h.resolveRaftWorkspace(w, r, req.WorkspaceSlug)
	if !ok {
		return
	}
	assigneeType, assigneeID, err := h.callerAsAssignee(r, callerID, ws)
	if err != nil {
		writeError(w, http.StatusForbidden, "caller cannot be assigned in this workspace")
		return
	}
	h.delegateIssueUpdate(w, r, ws, map[string]any{
		"assignee_type": assigneeType,
		"assignee_id":   assigneeID,
	})
}

// RaftSetIssueStatus moves an issue the caller already owns. Ownership means
// assignee or reporter: an agent may progress its own work, not reach into
// someone else's. Broader authority is a role question => Step 3.
func (h *Handler) RaftSetIssueStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		raftIssueScope
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Status) == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	ws, callerID, ok := h.resolveRaftWorkspace(w, r, req.WorkspaceSlug)
	if !ok {
		return
	}

	// Load the issue inside the resolved workspace so ownership is checked
	// against the real row, not against anything the caller asserted.
	q := r.URL.Query()
	q.Set("workspace_id", uuidToString(ws.ID))
	r.URL.RawQuery = q.Encode()
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}

	assigneeType, assigneeID, err := h.callerAsAssignee(r, callerID, ws)
	if err != nil {
		writeError(w, http.StatusForbidden, "caller cannot act in this workspace")
		return
	}
	// Caller identity is the (type, id) pair the server resolved above, so both
	// sides of these comparisons come from the server, never from the request.
	isAssignee := issue.AssigneeType.Valid && issue.AssigneeType.String == assigneeType &&
		issue.AssigneeID.Valid && uuidToString(issue.AssigneeID) == assigneeID
	isCreator := issue.CreatorType == assigneeType && uuidToString(issue.CreatorID) == assigneeID
	if !isAssignee && !isCreator {
		writeError(w, http.StatusForbidden, "only the assignee or the creator may change this issue's status")
		return
	}

	h.delegateIssueUpdate(w, r, ws, map[string]any{"status": req.Status})
}
