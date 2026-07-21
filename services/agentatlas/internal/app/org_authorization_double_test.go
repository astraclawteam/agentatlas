package app

import (
	"context"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

// allowOrgAuthorization is the permissive authorization double for tests whose
// subject is not authorization. Tests that exercise a denial supply their own
// double rather than relying on this one.
type allowOrgAuthorization struct {
	lastRequest nexus.BrowserAuthorizationRequest
	decision    string
	err         error
}

func (a *allowOrgAuthorization) AuthorizeTicketOperation(_ context.Context, _ string, req nexus.BrowserAuthorizationRequest) (nexus.BrowserAuthorizationDecision, error) {
	a.lastRequest = req
	if a.err != nil {
		return nexus.BrowserAuthorizationDecision{}, a.err
	}
	decision := a.decision
	if decision == "" {
		decision = "allow"
	}
	return nexus.BrowserAuthorizationDecision{Decision: decision, OrgVersion: req.OrgVersion}, nil
}
