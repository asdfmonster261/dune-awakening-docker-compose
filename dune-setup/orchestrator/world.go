package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// loadWorld reads world.yaml and builds the initial BattleGroup CR plus the
// ServerSetScale objects synthesized from spec.serverGroup.template.spec.sets.
func loadWorld(path string) (*BattleGroup, map[string]*ServerSetScale, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Use yaml.v3 to JSON-compatible interface{}. yaml.v3 produces
	// map[string]interface{} for mappings (unlike v2's map[interface{}]interface{}),
	// so subsequent json.Marshal works without conversion.
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}

	bg := &BattleGroup{
		APIVersion: getString(raw, "apiVersion"),
		Kind:       getString(raw, "kind"),
		Spec:       getMap(raw, "spec"),
		Status:     getMap(raw, "status"),
	}
	if md := getMap(raw, "metadata"); md != nil {
		bg.Metadata = Metadata{
			Name:      getString(md, "name"),
			Namespace: getString(md, "namespace"),
		}
	}

	if bg.APIVersion == "" {
		bg.APIVersion = "igw.funcom.com/v1"
	}
	if bg.Kind == "" {
		bg.Kind = "BattleGroup"
	}
	// world.yaml has no status section; the bg-director requires the key to
	// be present in the response, so default to an empty object.
	if bg.Status == nil {
		bg.Status = map[string]interface{}{}
	}

	scales := synthesizeScales(bg)
	return bg, scales, nil
}

// synthesizeScales walks spec.serverGroup.template.spec.sets[] and produces
// one ServerSetScale per map. The K8s server-operator emits these as a separate
// subresource; we emulate them here from the same source data.
//
// Name and label format mirror what the operator produces, because the
// bg-director derives map identity from these fields (it crashes with a
// null-key Dictionary.Add otherwise):
//   - metadata.name        = "<battlegroupId>-<safeMapName>"
//   - labels.role          = "igw-server"
//   - labels.battlegroup-id = <battlegroupId>
//   - annotations.map-name = <original MapName>
//   - spec.partitions      = the integer list from the set
//
// IMPORTANT: spec.partitions tracks what's CURRENTLY RUNNING, not what's
// *defined*. The bg-director's SetInstanceCount (BattlegroupUtils.Igwo.Api.cs)
// looks at `BattleGroup.spec...worldPartitions[]` minus `scale.spec.partitions`
// to find partitions it can ADD when scaling up. If we pre-fill partitions for
// on-demand maps (where world.yaml has no `partitions:` field), the director's
// SetInstanceCount bails with "Found no partitions not running" and never
// PATCHes replicas:0→1. So for on-demand maps the spec starts empty and the
// director fills it in on scale-up. For "container running but orchestrator
// just restarted" recovery, see recovery.go.
func synthesizeScales(bg *BattleGroup) map[string]*ServerSetScale {
	scales := make(map[string]*ServerSetScale)

	sg := getMap(bg.Spec, "serverGroup")
	if sg == nil {
		return scales
	}
	template := getMap(sg, "template")
	if template == nil {
		return scales
	}
	spec := getMap(template, "spec")
	if spec == nil {
		return scales
	}
	setsRaw, ok := spec["sets"].([]interface{})
	if !ok {
		return scales
	}

	for _, s := range setsRaw {
		set, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		mapName := getString(set, "map")
		if mapName == "" {
			continue
		}
		replicas := getInt(set, "replicas", 0)
		partitions := extractPartitions(set)
		fullName := fmt.Sprintf("%s-%s", bg.Metadata.Name, safeMapName(mapName))
		annotations := map[string]string{
			"igw.funcom.com/map-name": mapName,
		}
		// Mark on-demand sets so the crash-recovery sweeper can identify them
		// after the director PATCHes spec.replicas to 1. Annotations survive
		// scale PATCHes (director only touches spec.replicas/partitions), so
		// this is the stable on-demand marker across the lifecycle.
		if replicas == 0 {
			annotations["igw.funcom.com/on-demand"] = "true"
		}
		scales[fullName] = &ServerSetScale{
			APIVersion: "igw.funcom.com/v1",
			Kind:       "ServerSetScale",
			Metadata: Metadata{
				Name:      fullName,
				Namespace: bg.Metadata.Namespace,
				Labels: map[string]string{
					"role":                          "igw-server",
					"igw.funcom.com/battlegroup-id": bg.Metadata.Name,
				},
				Annotations: annotations,
			},
			Spec: map[string]interface{}{
				"replicas":   replicas,
				"partitions": partitions,
			},
			Status: map[string]interface{}{
				"replicas":      replicas,
				"readyReplicas": replicas,
			},
		}
	}
	return scales
}

// safeMapName mirrors utilities.GetSafeServerMapName from the server-operator:
// lowercase the string and turn underscores into dashes so the result is a
// valid K8s name segment (DNS label).
func safeMapName(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", "-"))
}

// extractPartitions reads set.partitions ([]any of ints) into []int. Returns
// an empty slice (not nil) so the JSON output is `[]` rather than `null`.
func extractPartitions(set map[string]interface{}) []int {
	out := []int{}
	raw, ok := set["partitions"].([]interface{})
	if !ok {
		return out
	}
	for _, p := range raw {
		switch n := p.(type) {
		case int:
			out = append(out, n)
		case int64:
			out = append(out, int(n))
		case float64:
			out = append(out, int(n))
		}
	}
	return out
}

// extractWorldPartitionIDs walks BattleGroup.spec.database.template.spec.deployment.
// spec.worldPartitions[] and builds a map[mapName][]int of partition IDs. This
// is the upstream source of truth that the K8s database-operator's
// reconcileWorldPartitions also reads to sync the postgres world_partition
// table. We use it as a fallback when a ServerSet has no explicit partitions
// (see synthesizeScales / Bug 2 comment). Returns an empty map if the path
// doesn't resolve.
func extractWorldPartitionIDs(bg *BattleGroup) map[string][]int {
	out := make(map[string][]int)
	db := getMap(bg.Spec, "database")
	if db == nil {
		return out
	}
	tmpl := getMap(db, "template")
	if tmpl == nil {
		return out
	}
	tmplSpec := getMap(tmpl, "spec")
	if tmplSpec == nil {
		return out
	}
	dep := getMap(tmplSpec, "deployment")
	if dep == nil {
		return out
	}
	depSpec := getMap(dep, "spec")
	if depSpec == nil {
		return out
	}
	wpsRaw, ok := depSpec["worldPartitions"].([]interface{})
	if !ok {
		return out
	}
	for _, w := range wpsRaw {
		entry, ok := w.(map[string]interface{})
		if !ok {
			continue
		}
		mapName := getString(entry, "map")
		if mapName == "" {
			continue
		}
		partsRaw, ok := entry["partitions"].([]interface{})
		if !ok {
			continue
		}
		ids := []int{}
		for _, p := range partsRaw {
			pe, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			id := getInt(pe, "id", -1)
			if id < 0 {
				continue
			}
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			out[mapName] = ids
		}
	}
	return out
}

// --- map helpers ---

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return nil
}

func getInt(m map[string]interface{}, key string, def int) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return def
}

// getStringMap extracts a JSON object of string values (e.g. labels, annotations)
// as map[string]string. Non-string values in the source are skipped. Returns
// nil if the key is absent or not an object, so callers can decide whether to
// leave the destination nil or default to an empty map.
func getStringMap(m map[string]interface{}, key string) map[string]string {
	raw, ok := m[key].(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// Sanity check: ensure the parsed CR can roundtrip to JSON. Used at startup
// to fail fast if world.yaml has YAML-only constructs.
func validateJSONable(bg *BattleGroup) error {
	_, err := json.Marshal(bg)
	return err
}
