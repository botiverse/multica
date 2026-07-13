package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/logger"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RaftLoginRequest is the "Login with Raft" callback payload: the one-time
// authorization code Raft issued (returnUrl?code=<code>) plus the redirect_uri
// used, mirroring GoogleLoginRequest.
type RaftLoginRequest struct {
	Code        string `json:"code"`
	RedirectURI string `json:"redirect_uri"`
}

type raftTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

// raftUserInfo mirrors the claims returned by Raft's GET /api/oauth/userinfo.
// Type is "human" or "agent"; Sub is the Raft user/agent id.
type raftUserInfo struct {
	Sub               string `json:"sub"`
	Type              string `json:"type"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	AvatarURL         string `json:"avatar_url"`
	Picture           string `json:"picture"`
	ServerID          string `json:"server_id"`
	ServerSlug        string `json:"server_slug"`
	ServerRole        string `json:"server_role"`
}

// raftAuthError carries an HTTP status alongside a client-safe message so both
// the JSON (RaftLogin) and browser-callback (RaftCallback) entry points can map
// a shared authentication failure onto the right response.
type raftAuthError struct {
	status int
	msg    string
}

func (e *raftAuthError) Error() string { return e.msg }

// raftConfigured reports whether Login with Raft is wired via env.
func raftAuthConfig() (clientID, clientSecret, baseURL string, ok bool) {
	clientID = os.Getenv("RAFT_OAUTH_CLIENT_ID")
	clientSecret = os.Getenv("RAFT_OAUTH_CLIENT_SECRET")
	baseURL = strings.TrimRight(os.Getenv("RAFT_OAUTH_BASE_URL"), "/")
	return clientID, clientSecret, baseURL, clientID != "" && clientSecret != "" && baseURL != ""
}

// authenticateRaft runs the shared Login-with-Raft exchange: swap the one-time
// code for a Raft access token, fetch identity claims, and resolve them to a
// Multica user-principal (creating it + the raft_identity link on first sight).
// Both the JSON login endpoint and the browser callback build on this.
func (h *Handler) authenticateRaft(ctx context.Context, code, redirectURI string) (db.User, bool, *raftAuthError) {
	clientID, clientSecret, baseURL, ok := raftAuthConfig()
	if !ok {
		return db.User{}, false, &raftAuthError{http.StatusServiceUnavailable, "Login with Raft is not configured"}
	}
	if redirectURI == "" {
		redirectURI = os.Getenv("RAFT_OAUTH_REDIRECT_URI")
	}

	// 1. Exchange the authorization code for a Raft access token. Client
	// credentials go in the Authorization header (HTTP Basic), which Raft's
	// token endpoint accepts.
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return db.User{}, false, &raftAuthError{http.StatusInternalServerError, "internal error"}
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.SetBasicAuth(clientID, clientSecret)

	tokenResp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		slog.Error("raft oauth token exchange failed", "error", err)
		return db.User{}, false, &raftAuthError{http.StatusBadGateway, "failed to exchange code with Raft"}
	}
	defer tokenResp.Body.Close()

	tokenBody, err := io.ReadAll(tokenResp.Body)
	if err != nil {
		return db.User{}, false, &raftAuthError{http.StatusBadGateway, "failed to read Raft token response"}
	}
	if tokenResp.StatusCode != http.StatusOK {
		slog.Error("raft oauth token exchange returned error", "status", tokenResp.StatusCode, "body", string(tokenBody))
		return db.User{}, false, &raftAuthError{http.StatusBadRequest, "failed to exchange code with Raft"}
	}

	var rToken raftTokenResponse
	if err := json.Unmarshal(tokenBody, &rToken); err != nil {
		return db.User{}, false, &raftAuthError{http.StatusBadGateway, "failed to parse Raft token response"}
	}
	if rToken.AccessToken == "" {
		return db.User{}, false, &raftAuthError{http.StatusBadGateway, "Raft token response had no access_token"}
	}

	// 2. Fetch identity claims.
	info, err := h.fetchRaftUserInfo(ctx, baseURL, rToken.AccessToken)
	if err != nil {
		slog.Error("raft userinfo fetch failed", "error", err)
		return db.User{}, false, &raftAuthError{http.StatusBadGateway, "failed to fetch user info from Raft"}
	}
	if info.Sub == "" || info.ServerID == "" {
		return db.User{}, false, &raftAuthError{http.StatusBadRequest, "Raft user info missing sub/server_id"}
	}

	// 3. Resolve to a Multica user through the raft_identity link.
	user, isNew, err := h.findOrCreateRaftUser(ctx, info)
	if err != nil {
		slog.Error("raft login: provision user failed", "error", err, "raft_sub", info.Sub)
		return db.User{}, false, &raftAuthError{http.StatusInternalServerError, "failed to provision user"}
	}
	return user, isNew, nil
}

// RaftLogin authenticates a Raft principal (human or agent) through "Login with
// Raft" and provisions/links a Multica user-principal for it, returning a JSON
// session (used by the web login page). Mirrors GoogleLogin.
//
// Multica personal access tokens are user-keyed, so every Raft principal —
// including agents — maps to a Multica user-principal here. Representing an
// agent as a first-class Multica agent entity is a separate, later step.
func (h *Handler) RaftLogin(w http.ResponseWriter, r *http.Request) {
	var req RaftLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Code == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	user, isNew, authErr := h.authenticateRaft(r.Context(), req.Code, req.RedirectURI)
	if authErr != nil {
		writeError(w, authErr.status, authErr.msg)
		return
	}
	if isNew {
		evt := analytics.Signup(uuidToString(user.ID), user.Email, signupSourceFromRequest(r))
		evt.Properties["auth_method"] = "raft"
		obsmetrics.RecordEvent(h.Analytics, h.Metrics, evt)
	}

	tokenString, err := h.issueJWT(user)
	if err != nil {
		slog.Warn("raft login failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	if err := auth.SetAuthCookies(w, tokenString); err != nil {
		slog.Warn("failed to set auth cookies", "error", err)
	}
	if h.CFSigner != nil {
		for _, cookie := range h.CFSigner.SignedCookies(time.Now().Add(72 * time.Hour)) {
			http.SetCookie(w, cookie)
		}
	}

	slog.Info("user logged in via raft",
		append(logger.RequestAttrs(r), "user_id", uuidToString(user.ID))...)
	writeJSON(w, http.StatusOK, LoginResponse{
		Token: tokenString,
		User:  userToResponse(user),
	})
}

func (h *Handler) fetchRaftUserInfo(ctx context.Context, baseURL, accessToken string) (raftUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/oauth/userinfo", nil)
	if err != nil {
		return raftUserInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return raftUserInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return raftUserInfo{}, fmt.Errorf("raft userinfo status %d: %s", resp.StatusCode, string(body))
	}
	var info raftUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return raftUserInfo{}, err
	}
	return info, nil
}

// findOrCreateRaftUser resolves a Raft principal to a Multica user through the
// raft_identity link table. On first sight it creates a user (with a synthetic,
// deterministic, non-routable email so the user.email UNIQUE/NOT NULL contract
// holds without inventing a real address) plus the link row in one transaction.
// isNew reports whether the user was created on this call.
func (h *Handler) findOrCreateRaftUser(ctx context.Context, info raftUserInfo) (user db.User, isNew bool, err error) {
	existing, err := h.Queries.GetRaftIdentity(ctx, db.GetRaftIdentityParams{
		RaftServerID: info.ServerID,
		RaftSub:      info.Sub,
	})
	if err == nil {
		// Known principal: refresh mutable display fields, return the user.
		_ = h.Queries.TouchRaftIdentity(ctx, db.TouchRaftIdentityParams{
			ID:            existing.ID,
			RaftUsername:  raftText(info.PreferredUsername),
			PrincipalType: raftPrincipalType(info.Type),
		})
		u, gerr := h.Queries.GetUser(ctx, existing.UserID)
		if gerr != nil {
			return db.User{}, false, gerr
		}
		return u, false, nil
	}
	if !isNotFound(err) {
		return db.User{}, false, err
	}

	// First sight: create the user + link atomically.
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return db.User{}, false, err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)

	created, err := qtx.CreateUser(ctx, db.CreateUserParams{
		Name:      raftDisplayName(info),
		Email:     raftSyntheticEmail(info),
		AvatarUrl: raftText(firstNonEmpty(info.AvatarURL, info.Picture)),
	})
	if err != nil {
		return db.User{}, false, err
	}
	if _, err = qtx.CreateRaftIdentity(ctx, db.CreateRaftIdentityParams{
		UserID:        created.ID,
		RaftServerID:  info.ServerID,
		RaftSub:       info.Sub,
		PrincipalType: raftPrincipalType(info.Type),
		RaftUsername:  raftText(info.PreferredUsername),
	}); err != nil {
		return db.User{}, false, err
	}

	// Login = joining this Raft server's shared Multica workspace (contract:
	// Multica workspace ≡ Raft server). The first principal from a server
	// creates + owns the workspace; later principals from the same server join
	// it as members. Membership is added only for principals that actually log
	// in — Multica does not mirror the full Raft member list.
	ws, role, err := resolveServerWorkspace(ctx, qtx, info)
	if err != nil {
		return db.User{}, false, err
	}
	if _, err = qtx.CreateMember(ctx, db.CreateMemberParams{
		WorkspaceID: ws.ID,
		UserID:      created.ID,
		Role:        role,
	}); err != nil {
		return db.User{}, false, err
	}

	// A Raft agent additionally becomes a first-class external agent in the
	// workspace: executed on Raft, not by Multica (runtime_mode='external',
	// runtime_id NULL, external_ref back to the Raft agent).
	if raftPrincipalType(info.Type) == "agent" {
		if _, err = qtx.CreateExternalAgent(ctx, db.CreateExternalAgentParams{
			WorkspaceID:      ws.ID,
			Name:             raftDisplayName(info),
			ExternalServerID: raftText(info.ServerID),
			ExternalAgentID:  raftText(info.Sub),
		}); err != nil {
			return db.User{}, false, err
		}
	}
	// Use the onboarded row as the returned user so the login response reflects
	// the committed onboarded state (not the pre-mark CreateUser snapshot).
	onboarded, err := qtx.MarkUserOnboarded(ctx, created.ID)
	if err != nil {
		return db.User{}, false, err
	}

	if err = tx.Commit(ctx); err != nil {
		return db.User{}, false, err
	}
	return onboarded, true, nil
}

// resolveServerWorkspace returns the Multica workspace for the Raft server the
// principal belongs to, creating it on first sight. role is "owner" when this
// call created the workspace, else "member" (the joining role). Runs inside the
// caller's transaction (qtx).
func resolveServerWorkspace(ctx context.Context, qtx *db.Queries, info raftUserInfo) (db.Workspace, string, error) {
	slug := raftServerWorkspaceSlug(info)
	ws, err := qtx.GetWorkspaceBySlug(ctx, slug)
	if err == nil {
		return ws, "member", nil
	}
	if !isNotFound(err) {
		return db.Workspace{}, "", err
	}
	name := raftServerWorkspaceName(info)
	created, err := qtx.CreateWorkspace(ctx, db.CreateWorkspaceParams{
		Name:        name,
		Slug:        slug,
		IssuePrefix: generateIssuePrefix(name),
	})
	if err != nil {
		return db.Workspace{}, "", err
	}
	return created, "owner", nil
}

// raftServerWorkspaceName / raftServerWorkspaceSlug derive a stable workspace
// identity from the Raft SERVER (not the principal), so every human and agent
// from one server converges on the same Multica workspace.
func raftServerWorkspaceName(info raftUserInfo) string {
	base := strings.TrimSpace(info.ServerSlug)
	if base == "" {
		base = "raft"
	}
	return base + " (Raft)"
}

func raftServerWorkspaceSlug(info raftUserInfo) string {
	base := slugifyRaft(info.ServerSlug)
	if base == "" {
		base = "raft"
	}
	suffix := slugifyRaft(info.ServerID)
	suffix = strings.ReplaceAll(suffix, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		suffix = "srv"
	}
	return base + "-" + suffix
}

// slugifyRaft lowercases and reduces a string to the workspace slug alphabet
// (^[a-z0-9]+(?:-[a-z0-9]+)*$): alnum runs joined by single hyphens, no
// leading/trailing hyphen.
func slugifyRaft(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		case b.Len() > 0 && !prevHyphen:
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// raftSyntheticEmail builds a stable, unique, non-routable address for a Raft
// principal. .invalid is reserved (RFC 2606) and can never receive mail, so
// these users cannot be confused with real email accounts.
func raftSyntheticEmail(info raftUserInfo) string {
	return fmt.Sprintf("%s.%s@%s.raft.invalid", raftPrincipalType(info.Type), info.Sub, info.ServerID)
}

func raftDisplayName(info raftUserInfo) string {
	if n := strings.TrimSpace(info.Name); n != "" {
		return n
	}
	if u := strings.TrimSpace(info.PreferredUsername); u != "" {
		return u
	}
	return "raft-" + info.Sub
}

func raftPrincipalType(t string) string {
	if t == "agent" {
		return "agent"
	}
	return "human"
}

func raftText(s string) pgtype.Text {
	s = strings.TrimSpace(s)
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
