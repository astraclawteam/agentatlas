package app

import (
	"context"
	"errors"
	"fmt"

	sdkworkflow "github.com/astraclawteam/agentatlas/sdk/go/workflow"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/workflow"
)

type publishedDreamWorkflowBinding struct {
	WorkflowID      string
	WorkflowVersion int32
	WorkflowName    string
	OutputSpaceID   string
	OutputName      string
}

type publishedDreamWorkflowBindingLister interface {
	ListPublishedDreamWorkflowBindings(context.Context, string, string, int32) ([]publishedDreamWorkflowBinding, error)
}

type workflowDreamBindingLister struct {
	workflows *workflow.Service
	orgs      browserSessionOrgStore
}

func (l workflowDreamBindingLister) ListPublishedDreamWorkflowBindings(ctx context.Context, enterpriseID, org string, limit int32) ([]publishedDreamWorkflowBinding, error) {
	if l.workflows == nil || l.orgs == nil || limit < 1 || limit > 100 {
		return nil, errors.New("published Dream workflow bindings unavailable")
	}
	spaces, err := l.orgs.ListBrowserKnowledgeSpacesByEnterprise(ctx, enterpriseID)
	if err != nil || len(spaces) > 1000 {
		return nil, errors.New("published Dream workflow output unavailable")
	}
	outputID, outputName := "", ""
	for _, space := range spaces {
		if space.EnterpriseID == enterpriseID && (space.OrgScope == org || space.OrgScope == "department:"+org || space.OrgScope == "project_group:"+org) {
			outputID, outputName = space.ID, safeKnowledgeSpaceName(space, org)
			break
		}
	}
	if outputID == "" {
		return []publishedDreamWorkflowBinding{}, nil
	}
	drafts, err := l.workflows.ListDrafts(ctx, enterpriseID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]publishedDreamWorkflowBinding, 0, len(drafts))
	for _, draft := range drafts {
		if draft.Kind == string(sdkworkflow.KindDream) && draft.LatestVersion > 0 {
			out = append(out, publishedDreamWorkflowBinding{WorkflowID: draft.ID, WorkflowVersion: draft.LatestVersion, WorkflowName: draft.Name, OutputSpaceID: outputID, OutputName: outputName})
		}
	}
	if len(out) > int(limit) {
		return nil, fmt.Errorf("published Dream workflow binding bound exceeded")
	}
	return out, nil
}
