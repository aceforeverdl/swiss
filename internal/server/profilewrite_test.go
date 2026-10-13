package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelsphere/swiss/internal/cluster"
	"github.com/modelsphere/swiss/internal/site"
)

// Writing the profile, which swissd owns: the chart creates none, so this is
// both the first-run setup and every later edit.
//
// profileServer is a swissd that can write to the cluster, with no login in
// the way -- what an edit does is the subject here, not who may do it.
func profileServer(t *testing.T, probe cluster.Probe) (*httptest.Server, *Server, *fakeWriter) {
	t.Helper()
	s := New(testConfig("prod-b300"), probe, discardLogger(), "test")
	w := &fakeWriter{}
	s.SetWriter(w)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, s, w
}

func put(t *testing.T, srv *httptest.Server, path string, body any) (int, map[string]any) {
	t.Helper()
	return do(t, http.DefaultClient, http.MethodPut, srv.URL+path, body)
}

// A site with no profile starts from the template, and what it starts from has
// to be a profile swissd would accept.
func TestProfileTemplateIsAProfile(t *testing.T) {
	srv, _, _ := profileServer(t, liveProbe())
	code, body := get(t, srv, "/api/profile/template")
	if code != 200 {
		t.Fatalf("status %d: %v", code, body)
	}
	yaml, _ := body["yaml"].(string)
	p, err := site.Parse([]byte(yaml), "template")
	if err != nil {
		t.Fatalf("the template must parse: %v", err)
	}
	if p.Name != "this-cluster" {
		t.Errorf("the template is a starting name the operator replaces, got %q", p.Name)
	}
	// The setup page opens on the form, which binds to an object rather than
	// to the text.
	parsed, ok := body["profile"].(map[string]any)
	if !ok {
		t.Fatalf("the template must carry the parsed profile too: %v", body)
	}
	if parsed["name"] != "this-cluster" {
		t.Errorf("parsed template: %v", parsed)
	}
}

// swissd owns the profile: a first install writes one through the same endpoint
// every later edit uses.
func TestSavingTheProfileWritesItAndComposesAgainstIt(t *testing.T) {
	bare := liveProbe()
	delete(bare.Maps, "swiss/site-profile")
	srv, s, w := profileServer(t, bare)

	body := "name: prod-b300\nnamespace: elsewhere\nmodel:\n  pathTemplate: /weights/{{name}}\n"
	code, out := put(t, srv, "/api/profile", map[string]string{"yaml": body})
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}

	stored, ok := w.written["swiss/site-profile"]
	if !ok {
		t.Fatalf("nothing was written; wrote %v", keys(w.written))
	}
	// Stored as submitted, comments and all: a round trip through the parsed
	// view would drop every comment in the file.
	if stored["profile.yaml"] != body {
		t.Errorf("stored text differs from what was sent:\n%q", stored["profile.yaml"])
	}

	// And in use immediately -- the cache is a TTL over a ConfigMap that just
	// changed, and composing against the old one was the point of the write.
	p, err := s.Profile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.Namespace != "elsewhere" {
		t.Errorf("swissd is still composing against the old profile: %+v", p)
	}
}

// A profile that does not parse would take the deploy form down until somebody
// edited a ConfigMap by hand. The request that would break it is where to say
// no.
func TestAnUnparseableProfileIsRefusedAndNotStored(t *testing.T) {
	srv, _, w := profileServer(t, liveProbe())

	for name, body := range map[string]string{
		"not yaml":           "name: [unclosed\n",
		"no name":            "model:\n  pathTemplate: /weights/{{name}}\n",
		"no path template":   "name: prod-b300\n",
		"unknown key":        "name: prod-b300\nmodel:\n  pathTemplate: /w\nnemspace: typo\n",
		"empty":              "   \n",
		"sites without url":  "name: prod-b300\nmodel:\n  pathTemplate: /w\nsites:\n  - name: dev\n",
		"sites without name": "name: prod-b300\nmodel:\n  pathTemplate: /w\nsites:\n  - url: https://swiss.dev.internal\n",
		"sites bad url":      "name: prod-b300\nmodel:\n  pathTemplate: /w\nsites:\n  - name: dev\n    url: swiss.dev.internal\n",
	} {
		code, out := put(t, srv, "/api/profile", map[string]string{"yaml": body})
		if code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%v)", name, code, out)
		}
	}
	if len(w.written) != 0 {
		t.Errorf("a refused profile must not be stored: %v", keys(w.written))
	}
}

// The editor edits text, so the endpoint that feeds it has to serve the text
// rather than only the parsed view.
func TestProfileCarriesTheStoredText(t *testing.T) {
	srv, _, _ := profileServer(t, liveProbe())
	code, body := get(t, srv, "/api/profile")
	if code != 200 {
		t.Fatalf("status %d: %v", code, body)
	}
	yaml, _ := body["yaml"].(string)
	if !strings.Contains(yaml, "name:") {
		t.Fatalf("the stored text is missing: %q", yaml)
	}
}

// A swissd reading its profile from a file has nothing to write to, and saying
// so beats writing a ConfigMap nobody reads.
func TestAFileProfileCannotBeEditedFromTheWeb(t *testing.T) {
	cfg := testConfig("prod-b300")
	cfg.Cluster.Profile.ConfigMap, cfg.Cluster.Profile.File = "", "/etc/swiss/profile.yaml"
	s := New(cfg, liveProbe(), discardLogger(), "test")
	s.SetWriter(&fakeWriter{})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	code, _ := put(t, srv, "/api/profile", map[string]string{"yaml": "name: x\nmodel:\n  pathTemplate: /w\n"})
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", code)
	}
}

// The form editor sends an object, not text: swissd renders it with the same
// library that reads it, so the document cannot be quoted wrong by a hand-built
// emitter in the browser.
func TestSavingTheProfileAsAnObjectRendersIt(t *testing.T) {
	bare := liveProbe()
	delete(bare.Maps, "swiss/site-profile")
	srv, s, w := profileServer(t, bare)

	code, out := put(t, srv, "/api/profile", map[string]any{
		"profile": map[string]any{
			"name":      "prod-b300",
			"namespace": "models",
			"model":     map[string]any{"pathTemplate": "/weights/{{name}}"},
			"route":     map[string]any{"nginxConfigMap": "llm-route/openresty-conf"},
			"sites": []map[string]string{
				{"name": "dev", "url": "https://swiss.dev.internal"},
			},
		},
	})
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}

	stored := w.written["swiss/site-profile"]["profile.yaml"]
	// A section nobody filled in is absent rather than rendered as `cache: {}`.
	for _, gone := range []string{"cache:", "scaler:", "schedule:", "registry:"} {
		if strings.Contains(stored, gone) {
			t.Errorf("an untouched section was written anyway (%s):\n%s", gone, stored)
		}
	}
	if !strings.Contains(stored, "nginxConfigMap: llm-route/openresty-conf") {
		t.Errorf("what was filled in is missing:\n%s", stored)
	}

	p, err := s.Profile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if p.Namespace != "models" || p.Route.NginxConfigMap == "" {
		t.Errorf("swissd is not composing against what was saved: %+v", p)
	}
	if len(p.Sites) != 1 || p.Sites[0].Name != "dev" || p.Sites[0].URL != "https://swiss.dev.internal" {
		t.Errorf("sites not saved: %+v", p.Sites)
	}
}

// An object still has to be a profile: the required fields are checked in one
// place, whichever editor arrived at it.
func TestAnObjectProfileIsValidatedToo(t *testing.T) {
	srv, _, w := profileServer(t, liveProbe())

	code, _ := put(t, srv, "/api/profile", map[string]any{
		"profile": map[string]any{"namespace": "models"},
	})
	if code != http.StatusBadRequest {
		t.Errorf("a profile with no name must be refused, got %d", code)
	}
	if len(w.written) != 0 {
		t.Errorf("nothing should have been stored: %v", keys(w.written))
	}
}

// Two editors, one document: sending both halves is a caller bug worth naming
// rather than a silent preference for one of them.
func TestSendingBothYAMLAndAnObjectIsRefused(t *testing.T) {
	srv, _, _ := profileServer(t, liveProbe())
	code, out := put(t, srv, "/api/profile", map[string]any{
		"yaml":    "name: a\nmodel:\n  pathTemplate: /w\n",
		"profile": map[string]any{"name": "b", "model": map[string]any{"pathTemplate": "/w"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status %d: %v", code, out)
	}
}

// GPU product aliases are set from the site page only: saved as part of the
// profile object, served back, and in force for the node view at once.
func TestSavingGPUProductAliasesFromTheSitePage(t *testing.T) {
	probe := liveProbe()
	delete(probe.Maps, "swiss/site-profile")
	probe.Nod = []cluster.Node{{
		Name: "npu-1", GPUResource: "huawei.com/Ascend910", GPUProduct: "huawei-Ascend910", GPUs: 8,
		Labels: map[string]string{"accelerator": "huawei-Ascend910"}, Schedulable: true, Ready: true,
	}}
	srv, _, w := profileServer(t, probe)
	product := func() any {
		t.Helper()
		_, body := get(t, srv, "/api/nodes")
		return body["nodes"].([]any)[0].(map[string]any)["GPUProduct"]
	}
	if got := product(); got != "huawei-Ascend910" {
		t.Fatalf("before any alias the label names the product, got %v", got)
	}

	code, out := put(t, srv, "/api/profile", map[string]any{
		"profile": map[string]any{
			"name":  "prod-b300",
			"model": map[string]any{"pathTemplate": "/weights/{{name}}"},
			"nodes": map[string]any{
				"gpusPerNode":       8,
				"gpuProductAliases": map[string][]string{" Ascend-910B3 ": {"accelerator=huawei-Ascend910"}},
			},
		},
	})
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}
	if stored := w.written["swiss/site-profile"]["profile.yaml"]; !strings.Contains(stored, "gpuProductAliases:") {
		t.Errorf("aliases were not written:\n%s", stored)
	}

	// The save answers with the parsed profile, which is what the page shows
	// next; the fake writer does not feed the probe, so GET cannot see it here.
	nodes := out["profile"].(map[string]any)["nodes"].(map[string]any)
	aliases, _ := nodes["gpuProductAliases"].(map[string]any)
	if sels, _ := aliases["Ascend-910B3"].([]any); len(sels) != 1 || sels[0] != "accelerator=huawei-Ascend910" {
		t.Errorf("the page reads back what it saved, trimmed: %v", nodes)
	}
	if got := product(); got != "Ascend-910B3" {
		t.Errorf("the node view must use the saved alias, got %v", got)
	}
}

// A bad selector is refused on save, naming the field, so the page can say
// which row is wrong.
func TestABadGPUProductAliasIsRefused(t *testing.T) {
	srv, _, w := profileServer(t, liveProbe())
	code, out := put(t, srv, "/api/profile", map[string]any{
		"profile": map[string]any{
			"name":  "prod-b300",
			"model": map[string]any{"pathTemplate": "/weights/{{name}}"},
			"nodes": map[string]any{"gpuProductAliases": map[string][]string{"Ascend-910B3": {"accelerator"}}},
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %v", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "nodes.gpuProductAliases.Ascend-910B3") {
		t.Errorf("the refusal should name the product: %q", msg)
	}
	if len(w.written) != 0 {
		t.Errorf("a refused profile must not be stored: %v", keys(w.written))
	}
}
