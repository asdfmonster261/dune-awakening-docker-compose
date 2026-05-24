package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// applyMergePatch applies an RFC 7396 JSON Merge Patch to a target. Both
// arguments are JSON-decoded values. The patch semantics are:
//   - For each key in the patch:
//   - If value is null, delete the key from target.
//   - If both target[key] and patch[key] are objects, recurse.
//   - Otherwise, replace target[key] with patch[key] (including arrays).
//   - Returns the resulting target.
func applyMergePatch(target, patch interface{}) interface{} {
	patchMap, ok := patch.(map[string]interface{})
	if !ok {
		// If the patch isn't an object, it replaces the target entirely.
		return patch
	}

	targetMap, ok := target.(map[string]interface{})
	if !ok {
		// Target isn't an object; per RFC 7396, treat as empty and apply patch.
		targetMap = make(map[string]interface{})
	}

	for k, v := range patchMap {
		if v == nil {
			delete(targetMap, k)
			continue
		}
		if _, isObj := v.(map[string]interface{}); isObj {
			targetMap[k] = applyMergePatch(targetMap[k], v)
			continue
		}
		targetMap[k] = v
	}
	return targetMap
}

// applyJSONPatch applies a subset of RFC 6902 (JSON Patch) operations to a
// JSON-decoded map. The bg-director sends json-patch when it needs to mutate
// multiple fields atomically — e.g. on player travel:
//
//	[{"op":"replace","path":"/spec/replicas","value":1},
//	 {"op":"replace","path":"/spec/partitions","value":[3]}]
//
// Supported ops: add, replace, remove. Paths follow RFC 6901 (JSON Pointer):
// leading slash, `/` separators, `~1` escapes `/`, `~0` escapes `~`, and `-`
// at the tail of an array path means "append". Unsupported ops return an
// error rather than silently ignoring — the director should learn that
// something is missing.
func applyJSONPatch(target map[string]interface{}, ops []map[string]interface{}) (map[string]interface{}, error) {
	for i, op := range ops {
		opName, _ := op["op"].(string)
		path, _ := op["path"].(string)
		value := op["value"]
		var err error
		switch opName {
		case "add":
			err = jsonPatchSet(target, path, value, true)
		case "replace":
			err = jsonPatchSet(target, path, value, false)
		case "remove":
			err = jsonPatchRemove(target, path)
		default:
			err = fmt.Errorf("unsupported op %q", opName)
		}
		if err != nil {
			return nil, fmt.Errorf("op #%d (%s %s): %w", i, opName, path, err)
		}
	}
	return target, nil
}

// jsonPointerSplit parses an RFC 6901 path into its decoded segments.
// "" → [], "/" → [""], "/spec/replicas" → ["spec","replicas"].
func jsonPointerSplit(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	if path[0] != '/' {
		return nil, fmt.Errorf("path must start with /")
	}
	raw := strings.Split(path[1:], "/")
	out := make([]string, len(raw))
	for i, seg := range raw {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		out[i] = seg
	}
	return out, nil
}

// jsonPatchSet writes value at the given path. If allowCreate is true,
// intermediate missing object keys are auto-vivified (RFC 6902 "add"
// semantics for nested missing parents).
func jsonPatchSet(target map[string]interface{}, path string, value interface{}, allowCreate bool) error {
	segs, err := jsonPointerSplit(path)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return fmt.Errorf("cannot replace whole document with a sub-path op")
	}
	var cur interface{} = target
	for i, seg := range segs {
		last := i == len(segs)-1
		switch v := cur.(type) {
		case map[string]interface{}:
			if last {
				v[seg] = value
				return nil
			}
			next, ok := v[seg]
			if !ok {
				if !allowCreate {
					return fmt.Errorf("missing key %q", seg)
				}
				m := map[string]interface{}{}
				v[seg] = m
				cur = m
			} else {
				cur = next
			}
		case []interface{}:
			idx, isAppend, err := arrayIndex(seg, len(v), allowCreate)
			if err != nil {
				return err
			}
			if last {
				if isAppend {
					// We can't mutate the parent slice's length from here —
					// the slice header lives in its container. arrayIndex
					// returns isAppend; the caller must handle append, but
					// we've recursed already. For our actual use we don't
					// receive append-to-existing-array ops; reject for now.
					return fmt.Errorf("'-' append within nested arrays not supported")
				}
				v[idx] = value
				return nil
			}
			cur = v[idx]
		default:
			return fmt.Errorf("cannot traverse into non-container at %q", seg)
		}
	}
	return nil
}

func jsonPatchRemove(target map[string]interface{}, path string) error {
	segs, err := jsonPointerSplit(path)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return fmt.Errorf("cannot remove whole document")
	}
	var cur interface{} = target
	for i, seg := range segs {
		last := i == len(segs)-1
		switch v := cur.(type) {
		case map[string]interface{}:
			if last {
				delete(v, seg)
				return nil
			}
			next, ok := v[seg]
			if !ok {
				return nil
			}
			cur = next
		default:
			return fmt.Errorf("cannot traverse into non-container at %q", seg)
		}
	}
	return nil
}

func arrayIndex(seg string, length int, allowAppend bool) (int, bool, error) {
	if seg == "-" {
		if !allowAppend {
			return 0, false, fmt.Errorf("'-' (append) only valid for add op")
		}
		return length, true, nil
	}
	n, err := strconv.Atoi(seg)
	if err != nil {
		return 0, false, fmt.Errorf("array index %q not a number", seg)
	}
	if n < 0 || n >= length {
		return 0, false, fmt.Errorf("array index %d out of bounds [0,%d)", n, length)
	}
	return n, false, nil
}

// applyPatch dispatches to applyMergePatch or applyJSONPatch based on the
// request's Content-Type. K8s clients pick the encoding per the operation
// they're performing — the bg-director uses merge-patch for simple replicas
// scaling and json-patch for compound mutations (e.g. replacing replicas AND
// partitions atomically on player travel).
func applyPatch(current map[string]interface{}, body []byte, contentType string) (map[string]interface{}, string, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return current, "merge", nil
	}
	if strings.HasPrefix(contentType, "application/json-patch+json") {
		var ops []map[string]interface{}
		if err := json.Unmarshal(body, &ops); err != nil {
			return nil, "json-patch", err
		}
		merged, err := applyJSONPatch(current, ops)
		return merged, "json-patch", err
	}
	var patch map[string]interface{}
	if err := json.Unmarshal(body, &patch); err != nil {
		return nil, "merge", err
	}
	merged, ok := applyMergePatch(current, patch).(map[string]interface{})
	if !ok {
		return nil, "merge", fmt.Errorf("merge result is not an object")
	}
	return merged, "merge", nil
}

// jsonRoundTrip marshals v to JSON then unmarshals back into a map. Useful for
// turning our typed structs into generic maps before applying a merge patch,
// then back out again.
func jsonRoundTrip(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return m, nil
}
