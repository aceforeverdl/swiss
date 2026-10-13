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

func schemaV1Message() string {
	return "LLMService backend requires catalog schema v2; this plan records schema v1; use the helm backend or migrate the catalog to v2"
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
	}{
		{"unset", v1ChartProbe()},
		{"explicit v1", explicitV1Probe(t)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := startRig(t, tc.probe, true, false, backendLLMSVC)
			hash := rig.storePlan(t, v1PlanFields(llmRelease))
			code, out := post(t, rig.srv, "/api/install", map[string]any{"planHash": hash})
			if code != http.StatusBadRequest || errText(out) != schemaV1Message() {
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
	if code != http.StatusBadRequest || errText(out) != schemaV1Message() {
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

// A rollback writes the archived plan. Its schema is the plan's, so renaming
// the catalog, or pointing the profile at a different repo, does not turn a
// stored v2 release into a v1 one.
func TestLLMRollbackKeepsV2AfterCatalogSwitch(t *testing.T) {
	rig := startRig(t, llmProbe(), true, true, backendLLMSVC)
	hash := rig.storePlan(t, map[string]any{"release": llmRelease})
	if code, out := post(t, rig.srv, "/api/install", map[string]any{"planHash": hash}); code != 200 {
		t.Fatalf("install %d %v", code, out)
	}
	next := rig.storePlan(t, map[string]any{
		"release": llmRelease, "overrides": map[string]any{"replicaCount": 4},
	})
	if code, out := post(t, rig.srv, "/api/apply", map[string]any{"planHash": next, "expectRevision": 1}); code != 200 {
		t.Fatalf("apply %d %v", code, out)
	}

	// The name the plan recorded is gone. The profile's only catalog is a
	// different repo, and it is v1.
	rig.s.cfg.Server.CacheTTL = 0
	rig.probe.Maps["swiss/site-profile"]["profile.yaml"] = profileYAML +
		"applyWith: llmsvc\nchartRepo: oci://ghcr.io/modelsphere/charts\ncatalogs:\n  - name: other\n    url: /var/empty/not-next\n    schemaVersion: v1\n"

	code, out := post(t, rig.srv, "/api/releases/models/"+llmRelease+"/rollback", map[string]any{
		"toRevision": 1, "expectRevision": 2, "note": "catalog moved",
	})
	if code != 200 {
		t.Fatalf("rollback of a stored v2 plan after the catalog was switched: %d %v", code, out)
	}
	if asNum(out["revision"]) != 3 || out["status"] != "applied" {
		t.Fatalf("response %v", out)
	}
}

// A stored v1 plan stays v1 even when the catalog name it records now points
// at a v2 repo. An absent schema annotation is v1, the same as an explicit one.
func TestLLMRollbackRefusesStoredV1Plan(t *testing.T) {
	for _, schema := range []string{"v1", ""} {
		name := schema
		if name == "" {
			name = "absent"
		}
		t.Run(name, func(t *testing.T) {
			rig := startRig(t, llmProbe(), true, true, backendLLMSVC)
			hash := rig.storePlan(t, map[string]any{"release": llmRelease})
			if code, out := post(t, rig.srv, "/api/install", map[string]any{"planHash": hash}); code != 200 {
				t.Fatalf("install %d %v", code, out)
			}
			before, err := rig.c.Get(t.Context(), "models", llmRelease)
			if err != nil {
				t.Fatal(err)
			}
			rewriteRevisionSchema(t, rig, "models", llmRelease, 1, schema)

			code, out := post(t, rig.srv, "/api/releases/models/"+llmRelease+"/rollback", map[string]any{
				"toRevision": 1, "expectRevision": 1,
			})
			if code != http.StatusBadRequest || errText(out) != schemaV1Message() {
				t.Fatalf("rollback %d %v", code, out)
			}
			got, err := rig.c.Get(t.Context(), "models", llmRelease)
			if err != nil {
				t.Fatal(err)
			}
			if specCanon(before) != specCanon(got) || got.GetGeneration() != before.GetGeneration() {
				t.Fatal("refused rollback mutated the object")
			}
			runs := rig.runs(t)
			if len(runs) != 1 || runs[0].Action != "install" {
				t.Fatalf("a refused rollback is not an audit row: %+v", runs)
			}
		})
	}
}
