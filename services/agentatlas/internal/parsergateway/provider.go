// Package parsergateway schedules parser sidecars behind the Parser Provider
// interface and normalizes their output toward AtlasDocument.
package parsergateway

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sdkparser "github.com/astraclawteam/agentatlas/sdk/go/parser"
	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
)

// ParseInput hands raw bytes to a provider. Bytes come from object storage;
// providers never touch enterprise data sources themselves.
type ParseInput struct {
	EnterpriseID string
	ArtifactID   string
	Filename     string
	ContentType  string
	Data         []byte
}

// ParseOutput is the provider-normalized fragment the artifact service
// assembles into a full AtlasDocument.
type ParseOutput struct {
	ProviderID    string
	Blocks        []atlasdocument.Block
	Tables        []atlasdocument.Table
	Images        []atlasdocument.ImageRegion
	AudioSegments []atlasdocument.AudioSegment
	VideoSegments []atlasdocument.VideoSegment
	Confidence    float64
}

// Provider is one parsing capability (docling, mineru, asr, video, plus
// enterprise extensions).
type Provider interface {
	Descriptor() sdkparser.Provider
	Parse(ctx context.Context, in ParseInput) (ParseOutput, error)
}

// Registry matches providers by hint or content type.
type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{providers: map[string]Provider{}}
	for _, p := range providers {
		d := p.Descriptor()
		if d.ProviderID == "" || d.OutputSchema != sdkparser.OutputSchemaV1 {
			return nil, fmt.Errorf("provider %q: invalid descriptor (output schema %q)", d.ProviderID, d.OutputSchema)
		}
		if _, dup := r.providers[d.ProviderID]; dup {
			return nil, fmt.Errorf("provider %q registered twice", d.ProviderID)
		}
		r.providers[d.ProviderID] = p
	}
	return r, nil
}

func (r *Registry) Get(providerID string) (Provider, error) {
	p, ok := r.providers[providerID]
	if !ok {
		return nil, fmt.Errorf("unknown parser provider %q", providerID)
	}
	return p, nil
}

// Resolve picks a provider: explicit hint wins; otherwise the first provider
// (stable order) whose input_types cover the content type.
func (r *Registry) Resolve(hint, contentType string) (Provider, error) {
	if hint != "" && hint != "auto" {
		return r.Get(hint)
	}
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, t := range r.providers[id].Descriptor().InputTypes {
			if matchContentType(t, contentType) {
				return r.providers[id], nil
			}
		}
	}
	return nil, fmt.Errorf("no parser provider accepts content type %q", contentType)
}

func matchContentType(pattern, contentType string) bool {
	if pattern == contentType {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "/*"); ok {
		return strings.HasPrefix(contentType, prefix+"/")
	}
	return false
}

// Capabilities lists every registered provider descriptor.
func (r *Registry) Capabilities() []sdkparser.Provider {
	out := make([]sdkparser.Provider, 0, len(r.providers))
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, r.providers[id].Descriptor())
	}
	return out
}
