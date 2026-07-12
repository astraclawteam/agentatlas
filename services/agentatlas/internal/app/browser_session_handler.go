package app

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
)

type browserActorKey struct{}
type browserSessionOrgStore interface {
	GetEnterprise(context.Context, string) (db.Enterprise, error)
	ListBrowserKnowledgeSpacesByEnterprise(context.Context, string) ([]db.KnowledgeSpace, error)
	ListOrgScopeBindingsByEnterprise(context.Context, string) ([]db.OrgScopeBinding, error)
}
type browserSessionHandler struct {
	sessions *browsersession.Service
	orgs     browserSessionOrgStore
}

type browserOrgNode struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Selectable bool              `json:"selectable"`
	Children   []*browserOrgNode `json:"children"`
}

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
		enterpriseName, orgTree := h.organizationPresentation(r.Context(), s)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "enterprise_id": s.EnterpriseID, "enterprise_name": enterpriseName, "enterprise_user_id": s.UserID, "display_name": s.DisplayName, "org_version": s.OrgVersion, "org_unit_ids": s.OrgUnitIDs, "org_tree": orgTree, "permissions": s.Permissions, "advanced_mode_allowed": s.AdvancedModeAllowed, "idle_expires_at": s.IdleExpiresAt, "absolute_expires_at": s.AbsoluteExpiresAt})
	})).ServeHTTP(w, r)
}

func (h *browserSessionHandler) organizationPresentation(ctx context.Context, session browsersession.Session) (string, []*browserOrgNode) {
	unknown := func() []*browserOrgNode {
		out := make([]*browserOrgNode, 0, len(session.OrgUnitIDs))
		for _, id := range session.OrgUnitIDs {
			out = append(out, &browserOrgNode{ID: id, Name: "未命名组织", Selectable: false, Children: []*browserOrgNode{}})
		}
		return out
	}
	if h.orgs == nil || len(session.OrgUnitIDs) > 1000 || session.OrgVersion < 1 {
		return "未命名企业", unknown()
	}
	enterprise, err := h.orgs.GetEnterprise(ctx, session.EnterpriseID)
	if err != nil {
		return "未命名企业", unknown()
	}
	enterpriseName := strings.TrimSpace(enterprise.Name)
	if enterpriseName == "" || enterpriseName == session.EnterpriseID {
		enterpriseName = "未命名企业"
	}
	spaces, err := h.orgs.ListBrowserKnowledgeSpacesByEnterprise(ctx, session.EnterpriseID)
	if err != nil || len(spaces) > 1000 {
		return enterpriseName, unknown()
	}
	bindings, err := h.orgs.ListOrgScopeBindingsByEnterprise(ctx, session.EnterpriseID)
	if err != nil || len(bindings) > 1000 {
		return enterpriseName, unknown()
	}

	spaceByID := make(map[string]db.KnowledgeSpace, len(spaces))
	spaceByScope := make(map[string]db.KnowledgeSpace, len(spaces))
	for _, space := range spaces {
		if space.EnterpriseID == session.EnterpriseID && space.OrgVersion <= session.OrgVersion {
			spaceByID[space.ID] = space
			spaceByScope[space.OrgScope] = space
		}
	}
	bindingByScope := make(map[string]db.OrgScopeBinding, len(bindings))
	candidatesByID := make(map[string][]string)
	for _, binding := range bindings {
		if binding.EnterpriseID != session.EnterpriseID {
			continue
		}
		if _, ok := spaceByID[binding.SpaceID]; !ok {
			continue
		}
		key := binding.ScopeKind + ":" + binding.ScopeID
		bindingByScope[key] = binding
		candidatesByID[binding.ScopeID] = append(candidatesByID[binding.ScopeID], key)
	}

	authorizedScope := make(map[string]string, len(session.OrgUnitIDs))
	unknownIDs := make([]string, 0)
	for _, sealedID := range session.OrgUnitIDs {
		resolved := ""
		if _, ok := spaceByScope[sealedID]; ok {
			resolved = sealedID
		} else if candidates := candidatesByID[sealedID]; len(candidates) == 1 {
			resolved = candidates[0]
		}
		if resolved == "" {
			unknownIDs = append(unknownIDs, sealedID)
			continue
		}
		authorizedScope[resolved] = sealedID
	}

	type presentationNode struct {
		node   *browserOrgNode
		parent string
	}
	nodes := make(map[string]*presentationNode)
	ensure := func(scope string) *presentationNode {
		if existing := nodes[scope]; existing != nil {
			return existing
		}
		space := spaceByScope[scope]
		name := strings.TrimSpace(space.Name)
		selectableID, selectable := authorizedScope[scope]
		if name == "" {
			name, selectable = "未命名组织", false
		}
		id := scope
		if selectable {
			id = selectableID
		}
		created := &presentationNode{node: &browserOrgNode{ID: id, Name: name, Selectable: selectable, Children: []*browserOrgNode{}}}
		nodes[scope] = created
		return created
	}
	for scope := range authorizedScope {
		current := scope
		seen := map[string]bool{}
		for depth := 0; current != "" && depth < 32 && !seen[current]; depth++ {
			seen[current] = true
			entry := ensure(current)
			binding, ok := bindingByScope[current]
			if !ok || !binding.ParentScopeKind.Valid || !binding.ParentScopeID.Valid {
				break
			}
			parent := binding.ParentScopeKind.String + ":" + binding.ParentScopeID.String
			if _, ok := spaceByScope[parent]; !ok {
				break
			}
			entry.parent = parent
			ensure(parent)
			current = parent
		}
	}
	for scope, entry := range nodes {
		if entry.parent != "" {
			parent := nodes[entry.parent]
			if parent != nil {
				parent.node.Children = append(parent.node.Children, nodes[scope].node)
			}
		}
	}
	roots := make([]*browserOrgNode, 0)
	for _, entry := range nodes {
		if entry.parent == "" {
			roots = append(roots, entry.node)
		}
	}
	for _, id := range unknownIDs {
		roots = append(roots, &browserOrgNode{ID: id, Name: "未命名组织", Selectable: false, Children: []*browserOrgNode{}})
	}
	var sortTree func([]*browserOrgNode)
	sortTree = func(items []*browserOrgNode) {
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		for _, item := range items {
			sortTree(item.Children)
		}
	}
	sortTree(roots)
	return enterpriseName, roots
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
		} else if forwarded := r.Header.Values("X-Forwarded-Proto"); len(forwarded) == 1 && strings.EqualFold(strings.TrimSpace(forwarded[0]), "https") {
			// Production terminates TLS at the ingress. A Secure session cookie is
			// never sent to a direct plaintext request, while the exact single-value
			// check avoids accepting an ambiguous proxy chain.
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
