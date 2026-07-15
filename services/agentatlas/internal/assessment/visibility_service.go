package assessment

// visibility_service.go wires the deterministic manager projection
// (ProjectForManager) to the request surface behind an OrgResolver PORT, so the
// served management BFF handler and the agent tool call ONE deterministic
// visibility service that resolves the real org facts and can never broaden
// scope. The tested, load-bearing authorization stays in visibility.go
// (ProjectForManager / AuthorizeManagerRead); this file only adapts a trusted
// caller identity + the authoritative org structure into a ManagerScopeRequest.

import (
	"context"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
)

// OrgResolver resolves the REAL org facts a manager-scope decision needs from the
// authoritative enterprise org structure (Task 0C operating map / org scope
// bindings) and the governed policy ownership. It is a PORT: production wires it
// over the org-scope-binding walk + the assessment-policy store; tests supply a
// deterministic double. It is NEVER fed by an LLM or a caller-supplied claim, so
// a caller can never inflate their own scope.
type OrgResolver interface {
	// PartyOf resolves an actor's org party (their unit + the strict-ancestor
	// OrgPath) at the current org version, from the authoritative org structure.
	PartyOf(ctx context.Context, tenant, userID string) (governance.Party, error)
	// CurrentOrgVersion returns the authoritative current org version for tenant.
	CurrentOrgVersion(ctx context.Context, tenant string) (int64, error)
	// OwnedPolicyKeys returns the assessment-policy keys the manager governs.
	OwnedPolicyKeys(ctx context.Context, tenant, managerID string) ([]string, error)
}

// ManagerVisibilityService turns a TRUSTED (tenant, managerID, assessmentID) into
// the manager detail — resolving the manager + subject org facts and the owned
// policies via the OrgResolver, then delegating the actual authorization + score
// disclosure to the deterministic ProjectForManager. There is no path here that
// can return the score/level without passing that authorization.
type ManagerVisibilityService struct {
	results ResultStore
	org     OrgResolver
}

// NewManagerVisibilityService constructs a ManagerVisibilityService.
func NewManagerVisibilityService(results ResultStore, org OrgResolver) *ManagerVisibilityService {
	return &ManagerVisibilityService{results: results, org: org}
}

// ManagerView returns the authorized manager detail for the assessment, or a
// fail-closed error (ErrNotFound for a missing/cross-tenant assessment;
// ErrManagerNotAuthorized / ErrPolicyNotOwned / ErrSubjectPartyMismatch when the
// exact hierarchy + policy authorization fails).
func (s *ManagerVisibilityService) ManagerView(ctx context.Context, tenant, managerID, assessmentID string) (ManagerAssessmentView, error) {
	wa, err := s.results.GetAssessment(ctx, tenant, assessmentID)
	if err != nil {
		return ManagerAssessmentView{}, err // ErrNotFound (cross-tenant reads too)
	}
	subject, err := s.org.PartyOf(ctx, tenant, wa.Subject)
	if err != nil {
		return ManagerAssessmentView{}, err
	}
	manager, err := s.org.PartyOf(ctx, tenant, managerID)
	if err != nil {
		return ManagerAssessmentView{}, err
	}
	orgVersion, err := s.org.CurrentOrgVersion(ctx, tenant)
	if err != nil {
		return ManagerAssessmentView{}, err
	}
	owned, err := s.org.OwnedPolicyKeys(ctx, tenant, managerID)
	if err != nil {
		return ManagerAssessmentView{}, err
	}
	return ProjectForManager(ManagerScopeRequest{
		Assessment:        wa,
		Manager:           manager,
		Subject:           subject,
		CurrentOrgVersion: orgVersion,
		OwnedPolicyKeys:   owned,
	})
}
