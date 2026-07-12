package app

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
)

type browserActorKey struct{}
type browserSessionHandler struct{ sessions *browsersession.Service }

func (h *browserSessionHandler) login(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "browser_session_unavailable", "browser sessions not configured")
		return
	}
	location, err := h.sessions.BeginLogin(r.Context(), r.URL.Query().Get("return_to"))
	if err != nil {
		slog.ErrorContext(r.Context(), "browser login start failed", "error", err)
		writeError(w, http.StatusBadRequest, "invalid_login", "login request is invalid")
		return
	}
	http.Redirect(w, r, location, http.StatusFound)
}
func (h *browserSessionHandler) callback(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "browser_session_unavailable", "browser sessions not configured")
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" || len(state) > 4096 || len(code) > 4096 {
		writeError(w, http.StatusBadRequest, "invalid_callback", "state and code are required")
		return
	}
	token, returnTo, err := h.sessions.CompleteLogin(r.Context(), state, code, "")
	if err != nil {
		slog.ErrorContext(r.Context(), "browser login callback failed", "error", err)
		writeError(w, http.StatusUnauthorized, "login_failed", "login could not be completed")
		return
	}
	if old, oldErr := r.Cookie("atlas_session"); oldErr == nil && old.Value != "" {
		if err := h.sessions.RevokeLocal(r.Context(), old.Value); err != nil {
			_ = h.sessions.RevokeLocal(r.Context(), token)
			slog.ErrorContext(r.Context(), "browser login session replacement failed", "error", err)
			writeError(w, http.StatusServiceUnavailable, "session_rotation_failed", "browser session could not be established")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "atlas_session", Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 0})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, returnTo, http.StatusFound)
}
func (h *browserSessionHandler) sessionGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.sessions == nil {
			writeError(w, http.StatusServiceUnavailable, "browser_session_unavailable", "browser sessions not configured")
			return
		}
		cookie, err := r.Cookie("atlas_session")
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
			return
		}
		access, err := h.sessions.Session(r.Context(), cookie.Value)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
			return
		}
		if access.ReplacementToken != "" {
			h.setSessionCookie(w, access.ReplacementToken)
		}
		ctx := context.WithValue(r.Context(), browserActorKey{}, access.Session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func browserActorFrom(ctx context.Context) (browsersession.Session, bool) {
	s, ok := ctx.Value(browserActorKey{}).(browsersession.Session)
	return s, ok
}
func (h *browserSessionHandler) session(w http.ResponseWriter, r *http.Request) {
	h.sessionGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, _ := browserActorFrom(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "enterprise_id": s.EnterpriseID, "enterprise_user_id": s.UserID, "display_name": s.DisplayName, "org_version": s.OrgVersion, "org_unit_ids": s.OrgUnitIDs, "permissions": s.Permissions, "advanced_mode_allowed": s.AdvancedModeAllowed, "idle_expires_at": s.IdleExpiresAt, "absolute_expires_at": s.AbsoluteExpiresAt})
	})).ServeHTTP(w, r)
}
func (h *browserSessionHandler) logout(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "browser_session_unavailable", "browser sessions not configured")
		return
	}
	cookie, err := r.Cookie("atlas_session")
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return
	}
	err = h.sessions.Logout(r.Context(), cookie.Value)
	http.SetCookie(w, &http.Cookie{Name: "atlas_session", Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	if err != nil {
		slog.ErrorContext(r.Context(), "browser logout pending reconciliation", "error", err)
		writeError(w, http.StatusAccepted, "logout_pending", "logout is being completed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *browserSessionHandler) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: "atlas_session", Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 0})
}
func sameOriginCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		scheme := "http"
		if r.TLS != nil || r.URL.Scheme == "https" {
			scheme = "https"
		}
		want := scheme + "://" + r.Host
		if origin == "" || !strings.EqualFold(strings.TrimRight(origin, "/"), want) {
			writeError(w, http.StatusForbidden, "csrf_denied", "same-origin request required")
			return
		}
		next.ServeHTTP(w, r)
	})
}
