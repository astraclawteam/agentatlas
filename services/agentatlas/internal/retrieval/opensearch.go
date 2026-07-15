// Package retrieval implements the OpenSearch-only runtime retrieval layer:
// per-enterprise indexes, smartcn keyword search, vector recall, client-side
// RRF hybrid merge, and llmrouter-routed embedding/rerank.
package retrieval

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/transportsecurity"
)

//go:embed mapping.json
var knowledgeMapping []byte

// SearchClient is the only doorway to OpenSearch (mockable in unit tests;
// integration tests run the real engine).
type SearchClient interface {
	EnsureIndex(ctx context.Context, index string) error
	Index(ctx context.Context, index, id string, doc any) error
	Search(ctx context.Context, index string, query any) (SearchResult, error)
}

type Hit struct {
	ID     string
	Score  float64
	Source json.RawMessage
}

type SearchResult struct {
	Total int
	Hits  []Hit
}

// IndexDocument is the searchable payload: summaries and sanitized snippets
// only — never raw originals.
type IndexDocument struct {
	EnterpriseID string `json:"enterprise_id"`
	SpaceID      string `json:"space_id,omitempty"`
	OrgScope     string `json:"org_scope,omitempty"`
	OrgVersion   int64  `json:"org_version,omitempty"`
	SourceType   string `json:"source_type"`
	// Authoritative marks a governed, evidence-grounded document (defense in
	// depth stamped at index time). Omitted (=> absent => non-authoritative)
	// for ungrounded digests such as the legacy dream_summary. The retrieval
	// layer additionally classifies by source_type so existing documents that
	// predate this field are still handled correctly (see authority.go).
	Authoritative     bool       `json:"authoritative,omitempty"`
	SummaryText       string     `json:"summary_text"`
	SanitizedSnippet  string     `json:"sanitized_snippet,omitempty"`
	Tags              []string   `json:"tags,omitempty"`
	NodeTime          *time.Time `json:"node_time,omitempty"`
	EvidencePointerID string     `json:"evidence_pointer_id,omitempty"`
	RiskLevel         string     `json:"risk_level,omitempty"`
	Embedding         []float32  `json:"embedding,omitempty"`
}

var indexNameSanitizer = regexp.MustCompile(`[^a-z0-9_-]+`)

// IndexName gives每企业独立索引: tenant isolation is enforced by index
// scoping first, enterprise_id filters second (defense in depth).
func IndexName(enterpriseID string) string {
	safe := indexNameSanitizer.ReplaceAllString(strings.ToLower(enterpriseID), "-")
	return "atlas-" + safe + "-knowledge"
}

// HTTPSearchClient talks to OpenSearch over its REST API.
type HTTPSearchClient struct {
	base string
	user string
	pass string
	http *http.Client
	// RefreshOnWrite makes writes immediately searchable (tests/small
	// deployments); leave false for bulk production indexing.
	RefreshOnWrite bool
}

var _ SearchClient = (*HTTPSearchClient)(nil)

// NewHTTPSearchClient dials OpenSearch. tlsMgr configures the OpenSearch
// link's transport security (services/agentatlas/internal/transportsecurity);
// nil, or a Manager built with LinkConfig.Mode == ModeOff, keeps today's
// plaintext behavior.
func NewHTTPSearchClient(addresses []string, username, password string, tlsMgr *transportsecurity.Manager) (*HTTPSearchClient, error) {
	if len(addresses) == 0 {
		return nil, fmt.Errorf("opensearch: no addresses configured")
	}
	transport := &http.Transport{}
	if tlsMgr != nil {
		if err := tlsMgr.ConfigureTransport(transport); err != nil {
			return nil, fmt.Errorf("opensearch tls: %w", err)
		}
	}
	return &HTTPSearchClient{
		base: strings.TrimRight(addresses[0], "/"),
		user: username, pass: password,
		http: &http.Client{Timeout: 60 * time.Second, Transport: transport},
	}, nil
}

func (c *HTTPSearchClient) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("opensearch %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func (c *HTTPSearchClient) EnsureIndex(ctx context.Context, index string) error {
	status, _, err := c.do(ctx, http.MethodHead, "/"+index, nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}
	status, raw, err := c.do(ctx, http.MethodPut, "/"+index, knowledgeMapping)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		// concurrent creation is fine
		if strings.Contains(string(raw), "resource_already_exists_exception") {
			return nil
		}
		return fmt.Errorf("opensearch create index %s: status %d: %s", index, status, truncate(raw, 300))
	}
	return nil
}

func (c *HTTPSearchClient) Index(ctx context.Context, index, id string, doc any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode doc: %w", err)
	}
	path := fmt.Sprintf("/%s/_doc/%s", index, id)
	if c.RefreshOnWrite {
		path += "?refresh=true"
	}
	status, body, err := c.do(ctx, http.MethodPut, path, raw)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("opensearch index %s/%s: status %d: %s", index, id, status, truncate(body, 300))
	}
	return nil
}

func (c *HTTPSearchClient) Search(ctx context.Context, index string, query any) (SearchResult, error) {
	raw, err := json.Marshal(query)
	if err != nil {
		return SearchResult{}, fmt.Errorf("encode query: %w", err)
	}
	status, body, err := c.do(ctx, http.MethodPost, "/"+index+"/_search", raw)
	if err != nil {
		return SearchResult{}, err
	}
	if status == http.StatusNotFound {
		return SearchResult{}, fmt.Errorf("opensearch: index %s does not exist", index)
	}
	if status < 200 || status > 299 {
		return SearchResult{}, fmt.Errorf("opensearch search %s: status %d: %s", index, status, truncate(body, 300))
	}
	var parsed struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				ID     string          `json:"_id"`
				Score  float64         `json:"_score"`
				Source json.RawMessage `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return SearchResult{}, fmt.Errorf("decode search response: %w", err)
	}
	out := SearchResult{Total: parsed.Hits.Total.Value}
	for _, h := range parsed.Hits.Hits {
		out.Hits = append(out.Hits, Hit{ID: h.ID, Score: h.Score, Source: h.Source})
	}
	return out, nil
}

func truncate(raw []byte, n int) string {
	s := string(raw)
	if len(s) > n {
		return s[:n]
	}
	return s
}
