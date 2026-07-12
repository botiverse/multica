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

// RaftLogin authenticates a Raft principal (human or agent) through "Login with
// Raft" and provisions/links a Multica user-principal for it.
//
// Flow mirrors GoogleLogin, pointed at the Raft OAuth server:
//  1. Exchange the one-time code at <RAFT_OAUTH_BASE_URL>/api/oauth/token
//     (grant_type=authorization_code) using this app's client credentials.
//  2. Fetch identity claims from /api/oauth/userinfo with the returned bearer.
//  3. Resolve the claims to a Multica user through the raft_identity link table,
//     creating the user + link on first sight, then issue a Multica session.
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

	clientID := os.Getenv("RAFT_OAUTH_CLIENT_ID")
	clientSecret := os.Getenv("RAFT_OAUTH_CLIENT_SECRET")
	baseURL := strings.TrimRight(os.Getenv("RAFT_OAUTH_BASE_URL"), "/")
	if clientID == "" || clientSecret == "" || baseURL == "" {
		writeError(w, http.StatusServiceUnavailable, "Login with Raft is not configured")
		return
	}

	redirectURI := req.RedirectURI
	if redirectURI == "" {
		redirectURI = os.Getenv("RAFT_OAUTH_REDIRECT_URI")
	}

	// 1. Exchange the authorization code for a Raft access token. Client
	// credentials go in the Authorization header (HTTP Basic), which Raft's
	// token endpoint accepts.
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {req.Code},
		"redirect_uri": {redirectURI},
	}
	tokenReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		baseURL+"/api/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.SetBasicAuth(clientID, clientSecret)

	tokenResp, err := http.DefaultClient.Do(tokenReq)
	if err != nil {
		slog.Error("raft oauth token exchange failed", "error", err)
		writeError(w, http.StatusBadGateway, "failed to exchange code with Raft")
		return
	}
	defer tokenResp.Body.Close()

	tokenBody, err := io.ReadAll(tokenResp.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read Raft token response")
		return
	}
	if tokenResp.StatusCode != http.StatusOK {
		slog.Error("raft oauth token exchange returned error", "status", tokenResp.StatusCode, "body", string(tokenBody))
		writeError(w, http.StatusBadRequest, "failed to exchange code with Raft")
		return
	}

	var rToken raftTokenResponse
	if err := json.Unmarshal(tokenBody, &rToken); err != nil {
		writeError(w, http.StatusBadGateway, "failed to parse Raft token response")
		return
	}
	if rToken.AccessToken == "" {
		writeError(w, http.StatusBadGateway, "Raft token response had no access_token")
		return
	}

	// 2. Fetch identity claims.
	info, err := h.fetchRaftUserInfo(r.Context(), baseURL, rToken.AccessToken)
	if err != nil {
		slog.Error("raft userinfo fetch failed", "error", err)
		writeError(w, http.StatusBadGateway, "failed to fetch user info from Raft")
		return
	}
	if info.Sub == "" || info.ServerID == "" {
		writeError(w, http.StatusBadRequest, "Raft user info missing sub/server_id")
		return
	}

	// 3. Resolve to a Multica user through the raft_identity link.
	user, isNew, err := h.findOrCreateRaftUser(r.Context(), info)
	if err != nil {
		slog.Error("raft login: provision user failed",
			append(logger.RequestAttrs(r), "error", err, "raft_sub", info.Sub)...)
		writeError(w, http.StatusInternalServerError, "failed to provision user")
		return
	}
	if isNew {
		evt := analytics.Signup(uuidToString(user.ID), user.Email, signupSourceFromRequest(r))
		evt.Properties["auth_method"] = "raft"
		obsmetrics.RecordEvent(h.Analytics, h.Metrics, evt)
	}

	tokenString, err := h.issueJWT(user)
	if err != nil {
		slog.Warn("raft login failed",
			append(logger.RequestAttrs(r), "error", err, "raft_sub", info.Sub)...)
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
		append(logger.RequestAttrs(r), "user_id", uuidToString(user.ID), "raft_sub", info.Sub, "principal_type", info.Type)...)
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
	if err = tx.Commit(ctx); err != nil {
		return db.User{}, false, err
	}
	return created, true, nil
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
