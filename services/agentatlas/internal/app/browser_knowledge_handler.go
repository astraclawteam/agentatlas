package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
	db "github.com/astraclawteam/agentatlas/services/agentatlas/db/generated"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

var errBrowserKnowledgeBound = errors.New("browser knowledge scope exceeds safe bound")

type browserKnowledgeStore interface {
	ListBrowserKnowledgeItems(context.Context, db.ListBrowserKnowledgeItemsParams) ([]db.ListBrowserKnowledgeItemsRow, error)
}

type browserKnowledgeChangeLister interface {
	List(context.Context, governance.Actor, string, int) ([]governance.Record, error)
}

type browserKnowledgeHandler struct {
	orgs       browserSessionOrgStore
	store      browserKnowledgeStore
	authorizer nexus.BrowserBFFClient
	changes    browserKnowledgeChangeLister
	now        func() time.Time
}

type browserKnowledgeItem struct {
	Key          string `json:"key"`
	Title        string `json:"title"`
	TypeLabel    string `json:"type_label"`
	UpdatedLabel string `json:"updated_label"`
	ScopeLabel   string `json:"scope_label"`
}

func (h *browserKnowledgeHandler) list(w http.ResponseWriter, r *http.Request) {
	session, ok := browserActorFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "no active browser session")
		return
	}
	orgUnitID := strings.TrimSpace(r.URL.Query().Get("org_unit_id"))
	rawQuery := r.URL.Query().Get("query")
	if !utf8.ValidString(rawQuery) || utf8.RuneCountInString(rawQuery) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_query", "search query must be at most 200 characters")
		return
	}
	query := strings.TrimSpace(rawQuery)
	if orgUnitID == "" || !containsExactOrganization(session.OrgUnitIDs, orgUnitID) {
		writeError(w, http.StatusForbidden, "forbidden", "knowledge scope is not authorized")
		return
	}
	if session.OrgVersion < 1 || session.UpstreamAccessToken == "" || h.authorizer == nil || h.orgs == nil || h.store == nil {
		writeError(w, http.StatusServiceUnavailable, "knowledge_unavailable", "knowledge workspace is unavailable")
		return
	}
	decision, err := h.authorizer.AuthorizeBrowserOperation(r.Context(), session.UpstreamAccessToken, nexus.BrowserAuthorizationRequest{
		OrgUnitID: orgUnitID, OrgVersion: session.OrgVersion, ResourceType: "knowledge_space", ResourceID: orgUnitID, Action: "knowledge.read",
	})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "knowledge authorization is unavailable")
		return
	}
	if decision.Decision != "allow" || decision.OrgVersion != session.OrgVersion || !containsExactOrganization(decision.OrgUnitIDs, orgUnitID) {
		writeError(w, http.StatusForbidden, "forbidden", "AgentNexus denied this knowledge scope")
		return
	}
	space, found, err := h.resolveSpace(r.Context(), session.EnterpriseID, orgUnitID, session.OrgVersion)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "knowledge_unavailable", "knowledge workspace is unavailable")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "knowledge scope was not found")
		return
	}
	rows, err := h.store.ListBrowserKnowledgeItems(r.Context(), db.ListBrowserKnowledgeItemsParams{
		EnterpriseID: session.EnterpriseID, SpaceID: space.ID, OrgScope: space.OrgScope, SearchQuery: escapeILikeLiteral(query), ResultLimit: 101,
	})
	if err != nil || len(rows) > 100 {
		writeError(w, http.StatusServiceUnavailable, "knowledge_unavailable", "knowledge workspace is unavailable")
		return
	}
	now := time.Now().UTC()
	if h.now != nil {
		now = h.now().UTC()
	}
	items := make([]browserKnowledgeItem, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.SummaryText) == "" || !row.NodeTime.Valid {
			continue
		}
		items = append(items, browserKnowledgeItem{
			Key: row.ID, Title: strings.TrimSpace(row.SummaryText), TypeLabel: knowledgeTypeLabel(row.SourceType),
			UpdatedLabel: relativeUpdateLabel(now, row.NodeTime.Time), ScopeLabel: safeKnowledgeSpaceName(db.KnowledgeSpace{ID: space.ID, Name: row.ScopeName, OrgScope: space.OrgScope}, orgUnitID),
		})
	}
	recent, reviews, countsAvailable := h.counts(r.Context(), session.EnterpriseID, session.UserID, orgUnitID, session.OrgVersion, session.OrgUnitIDs, session.Permissions, now)
	freshness := "还没有更新记录"
	if len(rows) > 0 && rows[0].NodeTime.Valid {
		freshness = relativeUpdateLabel(now, rows[0].NodeTime.Time)
	}
	var recentValue, reviewsValue any
	if countsAvailable {
		recentValue, reviewsValue = recent, reviews
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"organization": map[string]any{"name": safeKnowledgeSpaceName(space, orgUnitID)},
		"status":       map[string]any{"knowledge_runtime": "running", "freshness_label": freshness},
		"counts":       map[string]any{"available": countsAvailable, "recent_changes": recentValue, "reviews": reviewsValue},
		"items":        items,
	})
}

func (h *browserKnowledgeHandler) resolveSpace(ctx context.Context, enterpriseID, orgUnitID string, orgVersion int64) (db.KnowledgeSpace, bool, error) {
	spaces, err := h.orgs.ListBrowserKnowledgeSpacesByEnterprise(ctx, enterpriseID)
	if err != nil {
		return db.KnowledgeSpace{}, false, err
	}
	if len(spaces) > 1000 {
		return db.KnowledgeSpace{}, false, errBrowserKnowledgeBound
	}
	var match db.KnowledgeSpace
	matches := 0
	for _, space := range spaces {
		if space.EnterpriseID != enterpriseID || space.OrgVersion > orgVersion {
			continue
		}
		if space.OrgScope == orgUnitID || strings.HasSuffix(space.OrgScope, ":"+orgUnitID) {
			match = space
			matches++
		}
	}
	if matches > 1 {
		return db.KnowledgeSpace{}, false, errBrowserKnowledgeBound
	}
	return match, matches == 1, nil
}

func (h *browserKnowledgeHandler) counts(ctx context.Context, enterpriseID, userID, orgUnitID string, orgVersion int64, orgUnitIDs, permissions []string, now time.Time) (int, int, bool) {
	if h.changes == nil {
		return 0, 0, false
	}
	records, err := h.changes.List(ctx, governance.Actor{EnterpriseID: enterpriseID, UserID: userID, OrgVersion: orgVersion, OrgUnitIDs: orgUnitIDs, Permissions: permissions}, orgUnitID, 100)
	if err != nil {
		return 0, 0, false
	}
	recent, reviews := 0, 0
	for _, record := range records {
		if !record.Draft.UpdatedAt.Before(now.AddDate(0, 0, -30)) {
			recent++
		}
		if record.Draft.State == model.ChangeSubmitted && record.Route.State == model.RoutePending && eligibleKnowledgeReview(record, userID, permissions) {
			reviews++
		}
	}
	return recent, reviews, true
}

func eligibleKnowledgeReview(record governance.Record, userID string, permissions []string) bool {
	if record.Draft.RequesterUserID == userID || !hasKnowledgeApproval(permissions) {
		return false
	}
	switch record.Route.Mode {
	case model.ReviewUpward:
		return record.Route.ReviewerUserID == userID
	case model.ReviewAdminQueue:
		return record.Route.Queue != ""
	default:
		return false
	}
}

func escapeILikeLiteral(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

func knowledgeTypeLabel(sourceType string) string {
	switch sourceType {
	case "sop_change", "sop":
		return "SOP"
	case "artifact", "document", "document_import":
		return "资料"
	case "dream_summary":
		return "知识摘要"
	default:
		return "知识说明"
	}
}

func relativeUpdateLabel(now, updated time.Time) string {
	days := int(now.Sub(updated.UTC()).Hours() / 24)
	switch {
	case days <= 0:
		return "今天更新"
	case days == 1:
		return "昨天更新"
	case days < 30:
		return strings.TrimSpace(strconv.Itoa(days) + " 天前更新")
	default:
		return updated.UTC().Format("2006-01-02 更新")
	}
}

func safePresentationName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "未命名组织"
	}
	return name
}

func safeKnowledgeSpaceName(space db.KnowledgeSpace, sealedOrgUnitID string) string {
	name := safePresentationName(space.Name)
	if organizationNameIsIdentifier(name, space, db.OrgScopeBinding{}, sealedOrgUnitID) {
		return "未命名组织"
	}
	return name
}

func hasKnowledgeApproval(permissions []string) bool {
	for _, permission := range permissions {
		if permission == "approve_high_risk" {
			return true
		}
	}
	return false
}
