package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/browsersession"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/dream"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

type legacyWorkflowLister interface {
	ListDrafts(context.Context, string, int32) ([]workflow.DraftView, error)
}
type legacyDreamLister interface {
	ListPublishedBounded(context.Context, string, int32) ([]dream.PublishedPolicy, error)
}
type legacyTraceStore interface {
	ListRecentAnswerTraces(context.Context, string) ([]db.AnswerTrace, error)
}

type legacyBrowserHandler struct {
	authorizer nexus.BrowserBFFClient
	orgs       browserSessionOrgStore
	workflows  legacyWorkflowLister
	dreams     legacyDreamLister
	traces     legacyTraceStore
}

type legacyItem struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Detail string `json:"detail,omitempty"`
}

func (h *legacyBrowserHandler) read(surface string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := h.authorize(w, r, surface, surface+".read")
		if !ok {
			return
		}
		items, err := h.items(r.Context(), session, surface)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "capability_unavailable", "legacy capability has no safe browser-session adapter")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (h *legacyBrowserHandler) uploadAttachments(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorize(w, r, "assistant", "assistant.upload"); !ok {
		return
	}
	writeError(w, http.StatusServiceUnavailable, "capability_unavailable", "assistant attachment upload has no safe browser-session adapter")
}

func (h *legacyBrowserHandler) authorize(w http.ResponseWriter, r *http.Request, resource, action string) (browsersession.Session, bool) {
	session, ok := browserActorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return browsersession.Session{}, false
	}
	if !session.AdvancedModeAllowed {
		writeError(w, http.StatusForbidden, "advanced_mode_denied", "advanced maintenance is not authorized")
		return browsersession.Session{}, false
	}
	if h.authorizer == nil || session.UpstreamAccessToken == "" || session.OrgVersion < 1 || len(session.OrgUnitIDs) == 0 || len(session.OrgUnitIDs) > 1000 {
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "browser authorization is unavailable")
		return browsersession.Session{}, false
	}
	for _, org := range session.OrgUnitIDs {
		if strings.TrimSpace(org) == "" {
			continue
		}
		decision, err := h.authorizer.AuthorizeBrowserOperation(r.Context(), session.UpstreamAccessToken, nexus.BrowserAuthorizationRequest{
			OrgUnitID: org, OrgVersion: session.OrgVersion, ResourceType: "legacy_console", ResourceID: resource, Action: action,
		})
		if err == nil && decision.Decision == "allow" && decision.OrgVersion == session.OrgVersion && len(decision.OrgUnitIDs) > 0 {
			return session, true
		}
	}
	writeError(w, http.StatusForbidden, "forbidden", "AgentNexus denied this legacy capability")
	return browsersession.Session{}, false
}

var errLegacyUnavailable = errors.New("legacy capability unavailable")

func (h *legacyBrowserHandler) items(ctx context.Context, session browsersession.Session, surface string) ([]legacyItem, error) {
	switch surface {
	case "knowledge":
		if h.orgs == nil {
			return nil, errLegacyUnavailable
		}
		spaces, err := h.orgs.ListBrowserKnowledgeSpacesByEnterprise(ctx, session.EnterpriseID)
		if err != nil || len(spaces) > 1000 {
			return nil, errLegacyUnavailable
		}
		allowedScopes := authorizedLegacyScopes(session.OrgUnitIDs, spaces, session.OrgVersion)
		items := make([]legacyItem, 0)
		for _, space := range spaces {
			if allowedScopes[space.OrgScope] {
				label := strings.TrimSpace(space.Name)
				if label == "" {
					label = "未命名组织"
				}
				items = append(items, legacyItem{ID: space.ID, Label: label, Detail: "知识范围"})
			}
		}
		return boundedLegacyItems(items)
	case "dream":
		if h.dreams == nil || h.orgs == nil {
			return nil, errLegacyUnavailable
		}
		spaces, spaceErr := h.orgs.ListBrowserKnowledgeSpacesByEnterprise(ctx, session.EnterpriseID)
		if spaceErr != nil || len(spaces) > 1000 {
			return nil, errLegacyUnavailable
		}
		allowedScopes := authorizedLegacyScopes(session.OrgUnitIDs, spaces, session.OrgVersion)
		policies, err := h.dreams.ListPublishedBounded(ctx, session.EnterpriseID, 101)
		if err != nil {
			return nil, errLegacyUnavailable
		}
		items := make([]legacyItem, 0)
		for _, policy := range policies {
			if allowedScopes[policy.OrgScope] {
				items = append(items, legacyItem{ID: policy.ID, Label: "已发布梦境策略", Detail: policy.Status})
			}
		}
		return boundedLegacyItems(items)
	case "workflows":
		if h.workflows == nil {
			return nil, errLegacyUnavailable
		}
		drafts, err := h.workflows.ListDrafts(ctx, session.EnterpriseID, 100)
		if err != nil {
			return nil, errLegacyUnavailable
		}
		items := make([]legacyItem, 0, len(drafts))
		for _, draft := range drafts {
			items = append(items, legacyItem{ID: draft.ID, Label: draft.Name, Detail: "流程草稿"})
		}
		return boundedLegacyItems(items)
	case "evidence":
		if h.traces == nil || h.orgs == nil {
			return nil, errLegacyUnavailable
		}
		spaces, err := h.orgs.ListBrowserKnowledgeSpacesByEnterprise(ctx, session.EnterpriseID)
		if err != nil || len(spaces) > 1000 {
			return nil, errLegacyUnavailable
		}
		allowedSpaces := map[string]bool{}
		allowedScopes := authorizedLegacyScopes(session.OrgUnitIDs, spaces, session.OrgVersion)
		for _, space := range spaces {
			if allowedScopes[space.OrgScope] {
				allowedSpaces[space.ID] = true
			}
		}
		traces, err := h.traces.ListRecentAnswerTraces(ctx, session.EnterpriseID)
		if err != nil || len(traces) > 100 {
			return nil, errLegacyUnavailable
		}
		items := make([]legacyItem, 0)
		for _, trace := range traces {
			visible := false
			for _, spaceID := range trace.SpaceIds {
				if allowedSpaces[spaceID] {
					visible = true
					break
				}
			}
			if visible {
				items = append(items, legacyItem{ID: trace.ID, Label: trace.SanitizedQuestionSummary, Detail: "回答记录"})
			}
		}
		return boundedLegacyItems(items)
	default:
		return nil, errLegacyUnavailable
	}
}

func boundedLegacyItems(items []legacyItem) ([]legacyItem, error) {
	if len(items) > 100 {
		return nil, errLegacyUnavailable
	}
	return items, nil
}

func authorizedLegacyScopes(authorized []string, spaces []db.KnowledgeSpace, orgVersion int64) map[string]bool {
	result := map[string]bool{}
	for _, id := range authorized {
		matches := make([]string, 0, 1)
		for _, space := range spaces {
			if space.OrgVersion > orgVersion {
				continue
			}
			if space.OrgScope == id {
				matches = []string{space.OrgScope}
				break
			}
			if strings.HasSuffix(space.OrgScope, ":"+id) {
				matches = append(matches, space.OrgScope)
			}
		}
		if len(matches) == 1 {
			result[matches[0]] = true
		}
	}
	return result
}
