package server

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelsphere/swiss/internal/cluster"
)

func v1PlanFields(release string) map[string]any {
	return map[string]any{
		"model":        "qwen3.6-35b-a3b",
		"variant":      "sglang-tp2-h100",
		"chartVersion": "0.7.1",
		"release":      release,
	}
}

func schemaV1Message(repo string) string {
	return "LLMService backend requires catalog schema v2; catalog repo " + repo + " uses v1; use the helm backend or migrate the catalog to v2"
}

func explicitV1Probe(t *testing.T) cluster.Fake {
	t.Helper()
	root, err := filepath.Abs("../../../swiss-catalog")
	if err != nil {
		t.Fatal(err)
	}
	p := fakeProbe()
	p.Maps["swiss/site-profile"] = map[string]string{
		"profile.yaml": profileYAML + "chartRepo: oci://ghcr.io/modelsphere/charts\ncatalogs:\n  - name: public\n    url: " + root + "\n    schemaVersion: v1\n",
	}
	p.Secrets = map[string]map[string]string{}
	p.SecretLabels = map[string]map[string]string{}
	return p
}

func TestLLMRejectsCatalogSchemaV1(t *testing.T) {
	cases := []struct {
		name  string
		probe cluster.Fake
		repo  string
	}{
		{"unset", v1ChartProbe(), "default"},
		{"explicit v1", explicitV1Probe(t), "public"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := startRig(t, tc.probe, true, false, backendLLMSVC)
			hash := rig.storePlan(t, v1PlanFields(llmRelease))
			code, out := post(t, rig.srv, "/api/install", map[string]any{"planHash": hash})
			if code != http.StatusBadRequest || errText(out) != schemaV1Message(tc.repo) {
				t.Fatalf("install %d %v", code, out)
			}
			if rig.count(t) != 0 || len(rig.w.written) != 0 {
				t.Fatalf("rejected install wrote: objects %d cms %v", rig.count(t), rig.w.written)
			}
			if len(rig.runs(t)) != 0 {
				t.Fatal("a rejected install is not an audit row")
			}
		})
	}

	// An upgrade of a release the llmsvc backend already owns is the same gate,
	// and it returns before the object is updated.
	rig := startRig(t, v1ChartProbe(), true, false, backendLLMSVC)
	u := llmObject("models", llmRelease, map[string]string{"example.com/owner": "alice"})
	created, err := rig.c.Create(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	hash := rig.storePlan(t, v1PlanFields(llmRelease))
	code, out := post(t, rig.srv, "/api/apply", map[string]any{"planHash": hash})
	if code != http.StatusBadRequest || errText(out) != schemaV1Message("default") {
		t.Fatalf("upgrade %d %v", code, out)
	}
	got, err := rig.c.Get(t.Context(), "models", llmRelease)
	if err != nil {
		t.Fatal(err)
	}
	if specCanon(created) != specCanon(got) || got.GetGeneration() != created.GetGeneration() {
		t.Fatalf("upgrade mutated the object\nbefore %s\nafter  %s", specCanon(created), specCanon(got))
	}
	if got.GetAnnotations()["example.com/owner"] != "alice" {
		t.Fatalf("annotations %v", got.GetAnnotations())
	}
	if len(rig.runs(t)) != 0 {
		t.Fatal("a rejected upgrade is not an audit row")
	}
}

func TestHelmAcceptsCatalogSchemaV1AndV2(t *testing.T) {
	cases := []struct {
		name   string
		probe  cluster.Fake
		fields map[string]any
	}{
		{"v1", v1ChartProbe(), v1PlanFields(llmRelease)},
		{"v2", llmProbe(), map[string]any{"release": llmRelease}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := startRig(t, tc.probe, true, false, backendHelm)
			hash := rig.storePlan(t, tc.fields)
			code, out := post(t, rig.srv, "/api/install", map[string]any{"planHash": hash})
			if code == http.StatusBadRequest || strings.Contains(errText(out), "catalog schema") {
				t.Fatalf("helm refused %s: %d %v", tc.name, code, out)
			}
			if code != http.StatusInternalServerError {
				t.Fatalf("helmfile stub should fail the install, got %d %v", code, out)
			}
			files, ok := rig.w.written[planRef("models", llmRelease)]
			if !ok || files["plan.yaml"] == "" {
				t.Fatalf("helm did not record the plan: %v", rig.w.written)
			}
			if rig.count(t) != 0 {
				t.Fatal("helm install created an LLMService")
			}
		})
	}
}
