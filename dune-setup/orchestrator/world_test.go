package main

import (
	"reflect"
	"testing"
)

// makeBG returns a minimal BattleGroup matching the world.yaml shape — enough
// to exercise synthesizeScales. `sets` is the spec.serverGroup.template.spec.sets
// slice; `worldPartitions` is the spec.database.template.spec.deployment.spec.worldPartitions
// slice. Both passed in raw map form to match the YAML→interface{} parser output.
func makeBG(sets, worldPartitions []interface{}) *BattleGroup {
	return &BattleGroup{
		Metadata: Metadata{Name: "test-bg", Namespace: "ns"},
		Spec: map[string]interface{}{
			"database": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"deployment": map[string]interface{}{
							"spec": map[string]interface{}{
								"worldPartitions": worldPartitions,
							},
						},
					},
				},
			},
			"serverGroup": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"sets": sets,
					},
				},
			},
		},
	}
}

func TestSynthesizeScales_ExplicitPartitionsPreserved(t *testing.T) {
	bg := makeBG(
		[]interface{}{
			map[string]interface{}{
				"map":      "Survival_1",
				"replicas": 1,
				"partitions": []interface{}{
					1,
				},
			},
		},
		// worldPartitions also has the map, but the explicit set value should win.
		[]interface{}{
			map[string]interface{}{
				"map": "Survival_1",
				"partitions": []interface{}{
					map[string]interface{}{"id": 99},
				},
			},
		},
	)
	scales := synthesizeScales(bg)
	got := scales["test-bg-survival-1"].Spec["partitions"].([]int)
	if !reflect.DeepEqual(got, []int{1}) {
		t.Errorf("explicit set.partitions should be preserved, got %v", got)
	}
}

func TestSynthesizeScales_OnDemandStartsEmpty(t *testing.T) {
	// On-demand set: no partitions field. The director's SetInstanceCount
	// requires partitions to be empty so it can find one to add on scale-up.
	// (See Patch_SetInstanceCount: "Found no partitions not running" → bail.)
	// Recovery.go is responsible for restoring partitions when the container
	// is still running post-restart.
	bg := makeBG(
		[]interface{}{
			map[string]interface{}{
				"map":              "SH_Arrakeen",
				"replicas":         0,
				"dedicatedScaling": true,
			},
		},
		[]interface{}{
			map[string]interface{}{
				"map": "SH_Arrakeen",
				"partitions": []interface{}{
					map[string]interface{}{"id": 3, "dimension": 0},
				},
			},
		},
	)
	scales := synthesizeScales(bg)
	got := scales["test-bg-sh-arrakeen"].Spec["partitions"].([]int)
	if len(got) != 0 {
		t.Errorf("on-demand set must start with empty partitions so director "+
			"can find a partition to add on scale-up, got %v", got)
	}
	// The on-demand annotation must be set so the crash-recovery sweeper
	// can identify these scales after the director PATCHes replicas to 1.
	if got := scales["test-bg-sh-arrakeen"].Metadata.Annotations["igw.funcom.com/on-demand"]; got != "true" {
		t.Errorf("on-demand set must carry annotation igw.funcom.com/on-demand=true, got %q", got)
	}
}

func TestSynthesizeScales_AlwaysOnHasNoOnDemandAnnotation(t *testing.T) {
	// Always-on sets (replicas>0 in world.yaml) must NOT carry the on-demand
	// annotation, or the crash-recovery sweeper would incorrectly try to
	// scale them down when they die — compose owns their lifecycle via
	// restart: unless-stopped.
	bg := makeBG(
		[]interface{}{
			map[string]interface{}{
				"map":      "Survival_1",
				"replicas": 1,
				"partitions": []interface{}{
					1,
				},
			},
		},
		nil,
	)
	scales := synthesizeScales(bg)
	if _, present := scales["test-bg-survival-1"].Metadata.Annotations["igw.funcom.com/on-demand"]; present {
		t.Errorf("always-on set must not carry igw.funcom.com/on-demand annotation")
	}
}

func TestExtractWorldPartitionIDs_MultipleMaps(t *testing.T) {
	bg := makeBG(nil, []interface{}{
		map[string]interface{}{
			"map":        "Survival_1",
			"partitions": []interface{}{map[string]interface{}{"id": 1}},
		},
		map[string]interface{}{
			"map":        "SH_Arrakeen",
			"partitions": []interface{}{map[string]interface{}{"id": 3}},
		},
		map[string]interface{}{
			"map": "DeepDesert_1",
			"partitions": []interface{}{
				map[string]interface{}{"id": 8},
				map[string]interface{}{"id": 9},
			},
		},
	})
	got := extractWorldPartitionIDs(bg)
	want := map[string][]int{
		"Survival_1":   {1},
		"SH_Arrakeen":  {3},
		"DeepDesert_1": {8, 9},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractWorldPartitionIDs: got %v, want %v", got, want)
	}
}

func TestExtractWorldPartitionIDs_PathMissing(t *testing.T) {
	// BattleGroup with no .spec.database — should return empty map, not crash.
	bg := &BattleGroup{
		Metadata: Metadata{Name: "test"},
		Spec:     map[string]interface{}{},
	}
	got := extractWorldPartitionIDs(bg)
	if len(got) != 0 {
		t.Errorf("missing path should yield empty map, got %v", got)
	}
}
