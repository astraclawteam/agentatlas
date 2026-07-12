package governance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/governance"
)

var (
	ErrForbidden    = errors.New("governance: forbidden")
	ErrConflict     = errors.New("governance: revision conflict")
	ErrNotFound     = errors.New("governance: not found")
	ErrInvalidState = errors.New("governance: invalid state")
)

type Actor struct {
	EnterpriseID, UserID, DisplayName, UpstreamAccessToken string
	OrgVersion                                             int64
	OrgUnitIDs, Permissions                                []string
}
type SuggestionInput struct {
	OrgUnitID       string
	ResourceType    model.ResourceType
	ResourceID      string
	Action          model.Action
	BaseVersion     int32
	ProposedContent json.RawMessage
}
type DecisionInput struct{ Decision, Comment string }
type PublishedVersion struct {
	ChangeID, ResourceID, AuditRefID string
	Version                          int32
}
type ConflictError struct {
	CurrentRevision int32
	Diff            json.RawMessage
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s: current revision %d", ErrConflict, e.CurrentRevision)
}
func (e *ConflictError) Unwrap() error { return ErrConflict }

type Record struct {
	Draft                                 model.ChangeDraft
	Content                               json.RawMessage
	BaseContent                           json.RawMessage
	Assessment                            model.RiskAssessment
	Route                                 model.ReviewRoute
	Decision, DecisionBy, DecisionComment string
}
type PublishOperation struct {
	ChangeID    string
	Revision    int32
	PayloadHash string
	Result      PublishedVersion
	Complete    bool
}

type Store interface {
	Create(context.Context, Record) (Record, error)
	Get(context.Context, string, string) (Record, error)
	List(context.Context, string, string, int) ([]Record, error)
	Update(context.Context, string, string, int32, json.RawMessage) (Record, error)
	SaveReview(context.Context, string, Record) error
	BeginPublish(context.Context, string, string, string, int32, string) (PublishOperation, bool, error)
	FinalizePublish(context.Context, string, string, Actor, Record, string) (PublishedVersion, error)
}

type RouteResolver interface {
	Resolve(context.Context, Actor, Record, model.RiskAssessment) (model.ReviewRoute, error)
}
type authoritativeRouteResolver interface {
	RouteResolver
	Authoritative() bool
}
type AuditAppender interface {
	Append(context.Context, Actor, Record, string) (string, error)
}
type Publisher interface {
	Publish(context.Context, Actor, Record) (int32, error)
}
type Authorizer interface {
	Authorize(context.Context, Actor, string, model.ResourceType, string, string) error
}

type Service struct {
	store      Store
	routes     RouteResolver
	audit      AuditAppender
	publisher  Publisher
	authorizer Authorizer
	now        func() time.Time
}

func (s *Service) SetAuthorizer(a Authorizer) { s.authorizer = a }
func (s *Service) authorize(ctx context.Context, a Actor, org string, typ model.ResourceType, id, action string) error {
	if s.authorizer == nil {
		return nil
	}
	return s.authorizer.Authorize(ctx, a, org, typ, id, action)
}

func NewService(store Store, routes RouteResolver, audit AuditAppender, publisher Publisher, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, routes: routes, audit: audit, publisher: publisher, now: now}
}

func (s *Service) Suggest(ctx context.Context, actor Actor, in SuggestionInput) (model.ChangeDraft, error) {
	if !actor.has("suggest") || actor.EnterpriseID == "" || actor.UserID == "" || !actor.inOrg(in.OrgUnitID) || len(in.ProposedContent) == 0 || !json.Valid(in.ProposedContent) {
		return model.ChangeDraft{}, ErrForbidden
	}
	if err := s.authorize(ctx, actor, in.OrgUnitID, in.ResourceType, in.ResourceID, "suggest"); err != nil {
		return model.ChangeDraft{}, ErrForbidden
	}
	now := s.now().UTC()
	id := stableID("chg", actor.EnterpriseID, actor.UserID, in.ResourceID, fmt.Sprint(now.UnixNano()))
	d := model.ChangeDraft{ChangeID: id, EnterpriseID: actor.EnterpriseID, OrgUnitID: in.OrgUnitID, ResourceType: in.ResourceType, ResourceID: in.ResourceID, Action: in.Action, RequesterUserID: actor.UserID, Origin: model.OriginEmployeeSuggestion, PermissionMode: model.PermissionSuggestionOnly, Revision: 1, State: model.ChangeDraftState, BaseVersion: in.BaseVersion, CreatedAt: now, UpdatedAt: now}
	if err := json.Unmarshal(in.ProposedContent, &d.ProposedContent); err != nil || d.Validate() != nil {
		return model.ChangeDraft{}, fmt.Errorf("invalid suggestion")
	}
	rec, err := s.store.Create(ctx, Record{Draft: d, Content: clone(in.ProposedContent)})
	return rec.Draft, err
}

func (s *Service) CreateDraft(ctx context.Context, actor Actor, in SuggestionInput) (model.ChangeDraft, error) {
	if !actor.has("edit") || actor.EnterpriseID == "" || actor.UserID == "" || !actor.inOrg(in.OrgUnitID) || len(in.ProposedContent) == 0 || !json.Valid(in.ProposedContent) {
		return model.ChangeDraft{}, ErrForbidden
	}
	if err := s.authorize(ctx, actor, in.OrgUnitID, in.ResourceType, in.ResourceID, "edit"); err != nil {
		return model.ChangeDraft{}, ErrForbidden
	}
	now := s.now().UTC()
	id := stableID("chg", actor.EnterpriseID, actor.UserID, in.ResourceID, fmt.Sprint(now.UnixNano()))
	d := model.ChangeDraft{ChangeID: id, EnterpriseID: actor.EnterpriseID, OrgUnitID: in.OrgUnitID, ResourceType: in.ResourceType, ResourceID: in.ResourceID, Action: in.Action, RequesterUserID: actor.UserID, Origin: model.OriginDirectEdit, PermissionMode: model.PermissionDirectEdit, Revision: 1, State: model.ChangeDraftState, BaseVersion: in.BaseVersion, CreatedAt: now, UpdatedAt: now}
	if json.Unmarshal(in.ProposedContent, &d.ProposedContent) != nil || d.Validate() != nil {
		return model.ChangeDraft{}, ErrInvalidState
	}
	rec, err := s.store.Create(ctx, Record{Draft: d, Content: clone(in.ProposedContent)})
	return rec.Draft, err
}

func (s *Service) UpdateDraft(ctx context.Context, actor Actor, id string, revision int64, content json.RawMessage) (model.ChangeDraft, error) {
	if !actor.has("edit") || revision < 1 || len(content) == 0 || !json.Valid(content) {
		return model.ChangeDraft{}, ErrForbidden
	}
	rec, err := s.store.Get(ctx, actor.EnterpriseID, id)
	if err != nil {
		return model.ChangeDraft{}, err
	}
	if !actor.inOrg(rec.Draft.OrgUnitID) || rec.Draft.State != model.ChangeDraftState {
		return model.ChangeDraft{}, ErrForbidden
	}
	if err := s.authorize(ctx, actor, rec.Draft.OrgUnitID, rec.Draft.ResourceType, rec.Draft.ResourceID, "edit"); err != nil {
		return model.ChangeDraft{}, ErrForbidden
	}
	if int64(rec.Draft.Revision) != revision {
		return model.ChangeDraft{}, &ConflictError{CurrentRevision: rec.Draft.Revision, Diff: makeDiff(rec.Content, content)}
	}
	rec, err = s.store.Update(ctx, actor.EnterpriseID, id, int32(revision), content)
	return rec.Draft, err
}

func (s *Service) Get(ctx context.Context, actor Actor, id string) (Record, error) {
	rec, err := s.store.Get(ctx, actor.EnterpriseID, id)
	if err != nil {
		return Record{}, err
	}
	if !actor.inOrg(rec.Draft.OrgUnitID) {
		return Record{}, ErrNotFound
	}
	return rec, nil
}
func (s *Service) List(ctx context.Context, actor Actor, org string, limit int) ([]Record, error) {
	if !actor.inOrg(org) {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	return s.store.List(ctx, actor.EnterpriseID, org, limit)
}
func (s *Service) Diff(ctx context.Context, actor Actor, id string) (json.RawMessage, error) {
	rec, err := s.Get(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	return makeDiff(rec.BaseContent, rec.Content), nil
}

func (s *Service) Assess(ctx context.Context, actor Actor, id string) (model.RiskAssessment, error) {
	rec, err := s.Get(ctx, actor, id)
	if err != nil {
		return model.RiskAssessment{}, err
	}
	reasons := []string{}
	high := rec.Draft.ResourceType == model.ResourceWorkflow || rec.Draft.ResourceType == model.ResourceDreamPolicy
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(rec.Content, &fields)
	for _, f := range []string{"permissions", "approvals", "visibility", "masking_rules", "workflow", "nodes", "external_action", "org_unit_id"} {
		if _, ok := fields[f]; ok {
			high = true
			reasons = append(reasons, "changed "+f)
		}
	}
	if rec.Draft.ResourceType == model.ResourceWorkflow {
		reasons = append(reasons, "published workflow behavior")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "bounded content change")
	}
	sort.Strings(reasons)
	level := model.RiskLow
	if high {
		level = model.RiskHigh
	}
	return model.RiskAssessment{RiskLevel: level, RiskReasons: reasons}, nil
}

func (s *Service) Submit(ctx context.Context, actor Actor, id string) (model.ReviewRoute, error) {
	if !actor.has("edit") {
		return model.ReviewRoute{}, ErrForbidden
	}
	rec, err := s.Get(ctx, actor, id)
	if err != nil {
		return model.ReviewRoute{}, err
	}
	if rec.Draft.State != model.ChangeDraftState {
		return model.ReviewRoute{}, ErrInvalidState
	}
	if err := s.authorize(ctx, actor, rec.Draft.OrgUnitID, rec.Draft.ResourceType, rec.Draft.ResourceID, "submit"); err != nil {
		return model.ReviewRoute{}, ErrForbidden
	}
	assessment, err := s.Assess(ctx, actor, id)
	if err != nil {
		return model.ReviewRoute{}, err
	}
	var route model.ReviewRoute
	authoritative, _ := s.routes.(authoritativeRouteResolver)
	if (authoritative == nil || !authoritative.Authoritative()) && assessment.RiskLevel == model.RiskLow && actor.has("publish_low_risk") {
		route = model.ReviewRoute{ChangeID: id, ResourceType: rec.Draft.ResourceType, ResourceID: rec.Draft.ResourceID, RequesterUserID: rec.Draft.RequesterUserID, RiskLevel: model.RiskLow, Mode: model.ReviewSingleConfirmation, State: model.RoutePending, OrgPath: []string{}}
	} else {
		route, err = s.routes.Resolve(ctx, actor, rec, assessment)
		if err != nil {
			return model.ReviewRoute{}, err
		}
	}
	if err := route.Validate(); err != nil {
		return model.ReviewRoute{}, err
	}
	rec.Assessment = assessment
	rec.Route = route
	rec.Draft.State = model.ChangeSubmitted
	if err := s.store.SaveReview(ctx, actor.EnterpriseID, rec); err != nil {
		return model.ReviewRoute{}, err
	}
	return route, nil
}

func (s *Service) Decide(ctx context.Context, actor Actor, id string, in DecisionInput) error {
	if (in.Decision != "approve" && in.Decision != "reject") || len(in.Comment) > 4000 {
		return ErrInvalidState
	}
	rec, err := s.store.Get(ctx, actor.EnterpriseID, id)
	if err != nil {
		return err
	}
	if rec.Draft.State != model.ChangeSubmitted || rec.Route.State != model.RoutePending {
		return ErrInvalidState
	}
	if err := s.authorize(ctx, actor, rec.Draft.OrgUnitID, rec.Draft.ResourceType, rec.Draft.ResourceID, "decide"); err != nil {
		return ErrForbidden
	}
	if rec.Route.Mode == model.ReviewUpward {
		if actor.UserID == rec.Draft.RequesterUserID || actor.UserID != rec.Route.ReviewerUserID || !actor.has("approve_high_risk") {
			return ErrForbidden
		}
	} else if actor.UserID != rec.Draft.RequesterUserID || !actor.has("publish_low_risk") {
		return ErrForbidden
	}
	rec.Decision, rec.DecisionBy, rec.DecisionComment = in.Decision, actor.UserID, in.Comment
	if in.Decision == "approve" {
		rec.Draft.State = model.ChangeApproved
		rec.Route.State = model.RouteApproved
	} else {
		rec.Draft.State = model.ChangeRejected
		rec.Route.State = model.RouteRejected
	}
	return s.store.SaveReview(ctx, actor.EnterpriseID, rec)
}

func (s *Service) Publish(ctx context.Context, actor Actor, id, key string) (PublishedVersion, error) {
	if len(key) < 16 || len(key) > 128 || !actor.has("edit") {
		return PublishedVersion{}, ErrForbidden
	}
	rec, err := s.Get(ctx, actor, id)
	if err != nil {
		return PublishedVersion{}, err
	}
	if rec.Draft.State != model.ChangeApproved && rec.Draft.State != model.ChangePublished {
		return PublishedVersion{}, ErrInvalidState
	}
	if err := s.authorize(ctx, actor, rec.Draft.OrgUnitID, rec.Draft.ResourceType, rec.Draft.ResourceID, "publish"); err != nil {
		return PublishedVersion{}, ErrForbidden
	}
	payloadHash := stableID("", rec.Draft.ChangeID, fmt.Sprint(rec.Draft.Revision), string(rec.Content))
	op, existing, err := s.store.BeginPublish(ctx, actor.EnterpriseID, key, id, rec.Draft.Revision, payloadHash)
	if err != nil {
		return PublishedVersion{}, err
	}
	if existing {
		if op.PayloadHash != payloadHash {
			return PublishedVersion{}, ErrConflict
		}
		if op.Complete {
			return op.Result, nil
		}
	}
	if rec.Draft.State == model.ChangePublished {
		return PublishedVersion{}, ErrInvalidState
	}
	auditRef, err := s.audit.Append(ctx, actor, rec, key)
	if err != nil {
		return PublishedVersion{}, err
	}
	if s.publisher != nil {
		if _, err := s.publisher.Publish(ctx, actor, rec); err != nil {
			return PublishedVersion{}, err
		}
	}
	return s.store.FinalizePublish(ctx, actor.EnterpriseID, key, actor, rec, auditRef)
}

func (a Actor) has(permission string) bool {
	for _, p := range a.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}
func (a Actor) inOrg(org string) bool {
	for _, id := range a.OrgUnitIDs {
		if id == org {
			return true
		}
	}
	return false
}
func stableID(prefix string, values ...string) string {
	h := sha256.New()
	for _, v := range values {
		h.Write([]byte{0})
		h.Write([]byte(v))
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if prefix == "" {
		return sum
	}
	return prefix + "_" + sum[:24]
}
func clone(v []byte) []byte { return append([]byte(nil), v...) }
func makeDiff(before, after json.RawMessage) json.RawMessage {
	before = bytes.TrimSpace(before)
	after = bytes.TrimSpace(after)
	if len(before) == 0 {
		before = []byte("null")
	}
	if len(after) == 0 {
		after = []byte("null")
	}
	raw, _ := json.Marshal(map[string]any{"before": json.RawMessage(before), "after": json.RawMessage(after)})
	return raw
}

type StaticRouteResolver struct {
	ReviewerUserID string
	OrgPath        []string
}

func (r StaticRouteResolver) Resolve(_ context.Context, _ Actor, rec Record, assessment model.RiskAssessment) (model.ReviewRoute, error) {
	if r.ReviewerUserID == "" || r.ReviewerUserID == rec.Draft.RequesterUserID || len(r.OrgPath) == 0 {
		return model.ReviewRoute{}, ErrForbidden
	}
	return model.ReviewRoute{ChangeID: rec.Draft.ChangeID, ResourceType: rec.Draft.ResourceType, ResourceID: rec.Draft.ResourceID, RequesterUserID: rec.Draft.RequesterUserID, ReviewerUserID: r.ReviewerUserID, RiskLevel: assessment.RiskLevel, Mode: model.ReviewUpward, State: model.RoutePending, OrgPath: append([]string(nil), r.OrgPath...)}, nil
}
