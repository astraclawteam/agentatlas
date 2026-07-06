package app

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astraclawteam/agentatlas/sdk/go/atlasdocument"
	sdkparser "github.com/astraclawteam/agentatlas/sdk/go/parser"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/parsergateway"
)

// stubPDFProvider parses application/pdf into one block.
type stubPDFProvider struct{}

func (stubPDFProvider) Descriptor() sdkparser.Provider {
	return sdkparser.Provider{
		ProviderID: "docling", Capabilities: []string{"document"},
		InputTypes: []string{"application/pdf"}, OutputSchema: sdkparser.OutputSchemaV1,
		MaxFileSizeMB: 10, SupportsPrivate: true,
	}
}

func (stubPDFProvider) Parse(_ context.Context, in parsergateway.ParseInput) (parsergateway.ParseOutput, error) {
	return parsergateway.ParseOutput{
		Blocks: []atlasdocument.Block{{
			BlockID: "b1", Type: atlasdocument.BlockText,
			Text: "parsed:" + in.Filename, Order: 1,
		}},
		Confidence: 0.9,
	}, nil
}

func parseMultipart(t *testing.T, url, filename, contentType string, body []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("enterprise_id", "ent_1")
	_ = w.WriteField("artifact_id", "art_1")
	_ = w.WriteField("content_type", contentType)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/parse", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestParserServerRoutesAndFailsLoud(t *testing.T) {
	registry, err := parsergateway.NewRegistry(stubPDFProvider{})
	if err != nil {
		t.Fatal(err)
	}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // any HTTP response counts as reachable
	}))
	defer sidecar.Close()

	router := NewParserRouter(ParserRouterDeps{
		Gateway:  parsergateway.NewGateway(registry),
		Sidecars: map[string]string{"docling": sidecar.URL, "mineru": "http://127.0.0.1:1"},
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// PDF routes to the docling stub
	resp := parseMultipart(t, srv.URL, "sop.pdf", "application/pdf", []byte("%PDF-1.4"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("parse = %d", resp.StatusCode)
	}
	var parsed parsergateway.ParseResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if parsed.ProviderID != "docling" || len(parsed.Output.Blocks) != 1 || parsed.Output.Blocks[0].Text != "parsed:sop.pdf" {
		t.Fatalf("parse response = %+v", parsed)
	}

	// unknown content type fails loud
	resp = parseMultipart(t, srv.URL, "x.bin", "application/x-unknown", []byte{1, 2})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown content type = %d, want 422", resp.StatusCode)
	}
	resp.Body.Close()

	// health reports per-sidecar reachability
	hResp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		Sidecars map[string]bool `json:"sidecars"`
	}
	if err := json.NewDecoder(hResp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	hResp.Body.Close()
	if !health.Sidecars["docling"] || health.Sidecars["mineru"] {
		t.Fatalf("sidecar reachability = %v", health.Sidecars)
	}

	// the HTTP client round-trips the same contract
	client, err := parsergateway.NewHTTPClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Parse(context.Background(), "auto", parsergateway.ParseInput{
		EnterpriseID: "ent_1", ArtifactID: "art_2", Filename: "again.pdf",
		ContentType: "application/pdf", Data: []byte("%PDF-1.4"),
	})
	if err != nil {
		t.Fatalf("http client parse: %v", err)
	}
	if len(result.Output.Blocks) != 1 || result.Output.Blocks[0].Text != "parsed:again.pdf" {
		t.Fatalf("http client output = %+v", result.Output)
	}
	if _, err := client.Parse(context.Background(), "auto", parsergateway.ParseInput{
		Filename: "y.bin", ContentType: "application/x-unknown", Data: []byte{1},
	}); err == nil {
		t.Fatal("http client must surface parse failures loud")
	}
}
