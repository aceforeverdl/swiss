package server

import (
	"encoding/json"
	"testing"

	"github.com/modelsphere/swiss/internal/cluster"
)

// nodeSelector ANDs its labels, so it cannot express "any of these products".
// One term with one In expression is the OR, and the label key comes from the
// variant's vendor -- which is why the form sends product names, not affinity
// it had to build itself.
func TestGPUProductsBecomeNodeAffinity(t *testing.T) {
	srv, _ := deployServer(t, true)
	code, p := post(t, srv, "/api/plans", map[string]any{
		"model": "qwen3.6-35b-a3b", "serviceId": "r",
		"gpuProducts": []string{"NVIDIA-H100-80GB-HBM3", "NVIDIA-H800"},
	})
	if code != 200 {
		t.Fatalf("status %d: %v", code, p)
	}
	if _, present := planValues(p)["nodeSelector"]; present {
		t.Error("nodeSelector must not be written any more")
	}

	expr := matchExpr(t, p)
	if expr["key"] != "nvidia.com/gpu.product" || expr["operator"] != "In" {
		t.Fatalf("unexpected expression: %v", expr)
	}
	got := expr["values"].([]any)
	if len(got) != 2 || got[0] != "NVIDIA-H100-80GB-HBM3" || got[1] != "NVIDIA-H800" {
		t.Fatalf("both products must be offered as alternatives: %v", got)
	}
}

// Set at the nodeSelectorTerms path, so a preferred rule beside it survives.
func TestGPUProductsKeepAPreferredAffinityRule(t *testing.T) {
	srv, _ := deployServer(t, true)
	_, p := post(t, srv, "/api/plans", map[string]any{
		"model": "qwen3.6-35b-a3b", "serviceId": "r",
		"gpuProducts": []string{"NVIDIA-H800"},
		"overrides": map[string]any{
			"affinity": map[string]any{
				"nodeAffinity": map[string]any{
					"preferredDuringSchedulingIgnoredDuringExecution": []any{
						map[string]any{"weight": 1},
					},
				},
			},
		},
	})
	na := planValues(p)["affinity"].(map[string]any)["nodeAffinity"].(map[string]any)
	if na["preferredDuringSchedulingIgnoredDuringExecution"] == nil {
		t.Fatalf("a preferred rule beside the required one was dropped: %v", na)
	}
	if na["requiredDuringSchedulingIgnoredDuringExecution"] == nil {
		t.Fatal("the required rule is missing")
	}
}

func matchExpr(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	na := planValues(p)["affinity"].(map[string]any)["nodeAffinity"].(map[string]any)
	req := na["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	terms := req["nodeSelectorTerms"].([]any)
	if len(terms) != 1 {
		t.Fatalf("want one term, got %d", len(terms))
	}
	exprs := terms[0].(map[string]any)["matchExpressions"].([]any)
	if len(exprs) != 1 {
		t.Fatalf("want one expression, got %d", len(exprs))
	}
	return exprs[0].(map[string]any)
}

// The profile is where "what this cluster schedules GPU work on" belongs, and
// an explicit form value still wins.
func TestSchedulingDefaultsComeFromTheProfile(t *testing.T) {
	srv, _ := deployServer(t, true)
	_, p := post(t, srv, "/api/plans", map[string]any{
		"model": "qwen3.6-35b-a3b", "serviceId": "r",
	})
	vals := planValues(p)

	// testProfile carries no schedule block, so nothing is forced.
	if _, set := vals["schedulerName"]; set && layerOf(p, "schedulerName") != "site" {
		t.Errorf("an unset profile must not invent a scheduler: %v", vals["schedulerName"])
	}

	_, withForm := post(t, srv, "/api/plans", map[string]any{
		"model": "qwen3.6-35b-a3b", "serviceId": "r",
		"overrides": map[string]any{"schedulerName": "volcano"},
	})
	fv := planValues(withForm)
	if fv["schedulerName"] != "volcano" {
		t.Fatalf("the form must win: %v", fv["schedulerName"])
	}
	if got := layerOf(withForm, "schedulerName"); got != "form" {
		t.Errorf("an explicit value is the form's, whatever the profile said: %v", got)
	}
}

// nodeSelector, affinity and tolerations arrive as a YAML fragment, parsed on
// the server so there is one parser for it.
func TestSchedulingYAMLIsAccepted(t *testing.T) {
	srv, _ := deployServer(t, true)
	for _, frag := range []string{
		"tolerations:\n  - key: gpu\n    operator: Exists\n",
		"extraArgs: [--tp-size=8]\n", // the model entry set this too; the form wins
	} {
		code, body := post(t, srv, "/api/plans", map[string]any{
			"model": "qwen3.6-35b-a3b", "serviceId": "r", "overridesYAML": frag,
		})
		if code != 200 {
			t.Fatalf("%q: status %d: %v", frag, code, body)
		}
	}
}

// tolerations is a plain form-owned key, so the form sends the array straight
// through -- no vendor mapping, nothing for the server to translate.
func TestTolerationsFromTheFormLandInThePlan(t *testing.T) {
	srv, _ := deployServer(t, true)
	code, p := post(t, srv, "/api/plans", map[string]any{
		"model": "qwen3.6-35b-a3b", "serviceId": "r",
		"overrides": map[string]any{
			"tolerations": []any{
				map[string]any{"key": "gpu", "operator": "Exists", "effect": "NoSchedule"},
				map[string]any{"key": "tier", "operator": "Equal", "value": "spot"},
			},
		},
	})
	if code != 200 {
		t.Fatalf("status %d: %v", code, p)
	}
	tol, ok := planValues(p)["tolerations"].([]any)
	if !ok || len(tol) != 2 {
		t.Fatalf("both tolerations must survive: %v", planValues(p)["tolerations"])
	}
	first := tol[0].(map[string]any)
	if first["key"] != "gpu" || first["effect"] != "NoSchedule" {
		t.Errorf("unexpected toleration: %v", first)
	}
	if got := layerOf(p, "tolerations"); got != "form" {
		t.Errorf("tolerations are the form's: %v", got)
	}
}

// A deploy lands wherever inventory reports the product: one term per label
// key inventory reads, one per alias. Terms are ORed, an alias's labels ANDed.
func TestGPUProductTermsCoverInventoryKeysAndAliases(t *testing.T) {
	keys := cluster.ProductLabels(nil)["huawei.com/Ascend910"]
	aliases := map[string][]map[string]string{
		"Ascend-910B3": {{"accelerator": "huawei-Ascend910"}},
		"Ascend-910B4": {{"accelerator-type": "module-910b4-8", "accelerator": "huawei-Ascend910"}},
		"unrequested":  {{"x": "y"}},
	}
	got, _ := json.Marshal(gpuProductTerms(keys, aliases, []string{"Ascend-910B3", "Ascend-910B4"}))
	want := `[` +
		`{"matchExpressions":[{"key":"accelerator/huawei-ascend910","operator":"In","values":["Ascend-910B3","Ascend-910B4"]}]},` +
		`{"matchExpressions":[{"key":"accelerator","operator":"In","values":["Ascend-910B3","Ascend-910B4"]}]},` +
		`{"matchExpressions":[{"key":"accelerator","operator":"In","values":["huawei-Ascend910"]}]},` +
		`{"matchExpressions":[{"key":"accelerator","operator":"In","values":["huawei-Ascend910"]},{"key":"accelerator-type","operator":"In","values":["module-910b4-8"]}]}` +
		`]`
	if string(got) != want {
		t.Fatalf("terms:\n got %s\nwant %s", got, want)
	}
}

const aliasProfile = profileYAML + `nodes:
  gpuProductAliases:
    NVIDIA-H800:
      - gpu.example.com/model=h800
`

func TestProfileGPUProductAliasesReachThePlan(t *testing.T) {
	probe := liveProbe()
	probe.Maps["swiss/site-profile"] = map[string]string{"profile.yaml": aliasProfile}
	srv, _ := deployServerWith(t, probe, true)
	code, p := post(t, srv, "/api/plans", map[string]any{
		"model": "qwen3.6-35b-a3b", "serviceId": "r",
		"gpuProducts": []string{"NVIDIA-H800"},
	})
	if code != 200 {
		t.Fatalf("status %d: %v", code, p)
	}
	na := planValues(p)["affinity"].(map[string]any)["nodeAffinity"].(map[string]any)
	terms := na["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)["nodeSelectorTerms"].([]any)
	if len(terms) != 2 {
		t.Fatalf("want the product label term and the alias term, got %v", terms)
	}
	alias := terms[1].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)
	if alias["key"] != "gpu.example.com/model" || alias["values"].([]any)[0] != "h800" {
		t.Fatalf("unexpected alias term: %v", alias)
	}
}

func TestNodeViewReportsProfileAliases(t *testing.T) {
	probe := gpuProbe()
	probe.Maps["swiss/site-profile"] = map[string]string{"profile.yaml": aliasProfile}
	probe.Nod = []cluster.Node{{
		Name: "gpu-1", GPUResource: "nvidia.com/gpu", GPUProduct: "NVIDIA-Graphics-Device", GPUs: 8,
		Labels: map[string]string{"gpu.example.com/model": "h800"}, Schedulable: true, Ready: true,
	}}
	code, body := get(t, testServer(t, probe), "/api/nodes")
	if code != 200 {
		t.Fatalf("status %d: %v", code, body)
	}
	if got := body["nodes"].([]any)[0].(map[string]any)["GPUProduct"]; got != "NVIDIA-H800" {
		t.Fatalf("product %v, want the profile's alias", got)
	}
}
