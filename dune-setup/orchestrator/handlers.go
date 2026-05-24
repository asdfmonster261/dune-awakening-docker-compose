package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// API path patterns. CRDs live under /apis/igw.funcom.com/v1/...; core K8s
// resources (Nodes, Pods) live under /api/v1/... — run.sh calls those.
var (
	reBattleGroup     = regexp.MustCompile(`^/apis/igw\.funcom\.com/v1/namespaces/([^/]+)/battlegroups/([^/]+)$`)
	reScalesList      = regexp.MustCompile(`^/apis/igw\.funcom\.com/v1/namespaces/([^/]+)/serversetscales/?$`)
	reScaleOne        = regexp.MustCompile(`^/apis/igw\.funcom\.com/v1/namespaces/([^/]+)/serversetscales/([^/]+)$`)
	reServerStatsList = regexp.MustCompile(`^/apis/igw\.funcom\.com/v1/namespaces/([^/]+)/serverstats/?$`)
	reServerStats     = regexp.MustCompile(`^/apis/igw\.funcom\.com/v1/namespaces/([^/]+)/serverstats/([^/]+)$`)
	reDirectorStats   = regexp.MustCompile(`^/apis/igw\.funcom\.com/v1/namespaces/([^/]+)/battlegroupdirectorstats/([^/]+)$`)
	reNode          = regexp.MustCompile(`^/api/v1/nodes/([^/]+)$`)
	rePodOne        = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/pods/([^/]+)$`)
	rePodStatus     = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/pods/([^/]+)/status$`)
)

type Server struct {
	store  *Store
	docker *DockerClient // nil disables on-demand container start/stop
	hostIP string        // returned in Node.status.addresses to run.sh
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r, path)
	case http.MethodPatch:
		s.handlePatch(w, r, path)
	default:
		s.unhandled(w, r, "")
	}
}

// ---- GET handlers ----

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, path string) {
	if m := reBattleGroup.FindStringSubmatch(path); m != nil {
		s.getBattleGroup(w, r, m[1], m[2])
		return
	}
	// reScaleOne must be checked before reScalesList because the latter also
	// matches /serversetscales/ — reScaleOne requires a /{name} suffix.
	if m := reScaleOne.FindStringSubmatch(path); m != nil {
		s.getScale(w, r, m[1], m[2])
		return
	}
	if m := reScalesList.FindStringSubmatch(path); m != nil {
		s.listScales(w, r, m[1])
		return
	}
	// reServerStats (single) must be checked before reServerStatsList because
	// the latter's trailing-slash matches /serverstats/ but reServerStats
	// requires a name segment.
	if m := reServerStats.FindStringSubmatch(path); m != nil {
		s.getServerStats(w, r, m[1], m[2])
		return
	}
	if m := reServerStatsList.FindStringSubmatch(path); m != nil {
		s.listServerStats(w, r, m[1])
		return
	}
	if m := reNode.FindStringSubmatch(path); m != nil {
		s.getNode(w, r, m[1])
		return
	}
	if m := rePodOne.FindStringSubmatch(path); m != nil {
		s.getPod(w, r, m[1], m[2])
		return
	}
	s.unhandled(w, r, "")
}

func (s *Server) getBattleGroup(w http.ResponseWriter, r *http.Request, ns, name string) {
	bg := s.store.GetBattleGroup()
	if bg == nil || bg.Metadata.Name != name {
		s.notFound(w, r, "battlegroups", name)
		return
	}
	logGET(r, "battlegroups", name)
	writeJSON(w, http.StatusOK, bg)
}

func (s *Server) getScale(w http.ResponseWriter, r *http.Request, ns, name string) {
	sc := s.store.GetScale(name)
	if sc == nil {
		s.notFound(w, r, "serversetscales", name)
		return
	}
	logGET(r, "serversetscales", name)
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) listScales(w http.ResponseWriter, r *http.Request, ns string) {
	items := s.store.ListScales()
	list := ServerSetScaleList{
		APIVersion: "igw.funcom.com/v1",
		Kind:       "ServerSetScaleList",
		Metadata:   ListMetadata{ResourceVersion: s.store.ListResourceVersion()},
		Items:      items,
	}
	logGET(r, "serversetscales", fmt.Sprintf("(list, %d items)", len(items)))
	writeJSON(w, http.StatusOK, list)
}

// getServerStats / listServerStats return what game-servers have PATCHed.
// Used by dune-admin's Battlegroup status tab (and potentially anything
// else that wants to read live server runtime state). Note the store only
// retains what's been PATCHed; servers that have never reported (e.g.
// on-demand maps that haven't scaled up) won't appear here.
func (s *Server) getServerStats(w http.ResponseWriter, r *http.Request, ns, name string) {
	stats := s.store.GetServerStats(name)
	if stats == nil {
		s.notFound(w, r, "serverstats", name)
		return
	}
	logGET(r, "serverstats", name)
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) listServerStats(w http.ResponseWriter, r *http.Request, ns string) {
	items := s.store.ListServerStats()
	list := map[string]interface{}{
		"apiVersion": "igw.funcom.com/v1",
		"kind":       "ServerStatsList",
		"metadata":   ListMetadata{ResourceVersion: s.store.ListResourceVersion()},
		"items":      items,
	}
	logGET(r, "serverstats", fmt.Sprintf("(list, %d items)", len(items)))
	writeJSON(w, http.StatusOK, list)
}

// ---- PATCH handlers ----

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request, path string) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()

	if m := reServerStats.FindStringSubmatch(path); m != nil {
		s.patchServerStats(w, r, m[1], m[2], body)
		return
	}
	if m := reDirectorStats.FindStringSubmatch(path); m != nil {
		s.patchDirectorStats(w, r, m[1], m[2], body)
		return
	}
	if m := reBattleGroup.FindStringSubmatch(path); m != nil {
		s.patchBattleGroup(w, r, m[1], m[2], body)
		return
	}
	if m := reScaleOne.FindStringSubmatch(path); m != nil {
		s.patchScale(w, r, m[1], m[2], body)
		return
	}
	// rePodStatus must precede rePodOne — both end in /pods/{name} otherwise.
	if m := rePodStatus.FindStringSubmatch(path); m != nil {
		s.patchPodStatus(w, r, m[1], m[2], body)
		return
	}
	if m := rePodOne.FindStringSubmatch(path); m != nil {
		s.patchPodStatus(w, r, m[1], m[2], body)
		return
	}
	s.unhandled(w, r, string(body))
}

// 3a: serverstats and battlegroupdirectorstats PATCHes — store, ack with echo.

func (s *Server) patchServerStats(w http.ResponseWriter, r *http.Request, ns, name string, body []byte) {
	parsed, err := parsePatchBody(body)
	if err != nil {
		log.Printf("[%s] PATCH serverstats/%s: bad body: %v", ns, name, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.store.SetServerStats(name, parsed)
	log.Printf("[%s] PATCH serverstats/%s — %s", ns, name, summarizeServerStats(parsed))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"apiVersion": "igw.funcom.com/v1",
		"kind":       "ServerStats",
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       ns,
			"resourceVersion": s.store.ListResourceVersion(),
		},
	})
}

func (s *Server) patchDirectorStats(w http.ResponseWriter, r *http.Request, ns, name string, body []byte) {
	parsed, err := parsePatchBody(body)
	if err != nil {
		log.Printf("[%s] PATCH battlegroupdirectorstats/%s: bad body: %v", ns, name, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.store.SetDirectorStats(name, parsed)
	log.Printf("[%s] PATCH battlegroupdirectorstats/%s — %s", ns, name, summarizeDirectorStats(parsed))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"apiVersion": "igw.funcom.com/v1",
		"kind":       "BattleGroupDirectorStats",
		"metadata": map[string]interface{}{
			"name":            name,
			"namespace":       ns,
			"resourceVersion": s.store.ListResourceVersion(),
		},
	})
}

// 3c: PATCH on BattleGroup and ServerSetScale — apply merge patch, log diff,
// return updated object. We do NOT act on these patches yet — purely observe.

func (s *Server) patchBattleGroup(w http.ResponseWriter, r *http.Request, ns, name string, body []byte) {
	bg := s.store.GetBattleGroup()
	if bg == nil || bg.Metadata.Name != name {
		s.notFound(w, r, "battlegroups", name)
		return
	}

	current, err := jsonRoundTrip(bg)
	if err != nil {
		log.Printf("[%s] PATCH battlegroups/%s: marshal current: %v", ns, name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	merged, kind, err := applyPatch(current, body, r.Header.Get("Content-Type"))
	if err != nil {
		log.Printf("[%s] PATCH battlegroups/%s: %s parse/apply failed: %v — raw=%q",
			ns, name, kind, err, string(body))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	updated, err := mapToBattleGroup(merged)
	if err != nil {
		log.Printf("[%s] PATCH battlegroups/%s: rebuild: %v", ns, name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.store.SetBattleGroup(updated)

	log.Printf("[%s] PATCH battlegroups/%s — applied (%s)", ns, name, kind)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) patchScale(w http.ResponseWriter, r *http.Request, ns, name string, body []byte) {
	sc := s.store.GetScale(name)
	if sc == nil {
		s.notFound(w, r, "serversetscales", name)
		return
	}

	oldReplicas := getInt(sc.Spec, "replicas", 0)

	current, err := jsonRoundTrip(sc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	merged, kind, err := applyPatch(current, body, r.Header.Get("Content-Type"))
	if err != nil {
		log.Printf("[%s] PATCH serversetscales/%s: %s parse/apply failed: %v — raw=%q",
			ns, name, kind, err, string(body))
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	updated, err := mapToScale(merged)
	if err != nil {
		log.Printf("[%s] PATCH serversetscales/%s: rebuild: %v", ns, name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	newReplicas := getInt(updated.Spec, "replicas", 0)

	// Reflect desired replicas in status optimistically. The game-server's own
	// serverstats PATCH (handled separately) is the source of truth for runtime
	// readiness; this just keeps the director's view of `replicas` consistent.
	if updated.Status == nil {
		updated.Status = map[string]interface{}{}
	}
	updated.Status["replicas"] = newReplicas
	updated.Status["readyReplicas"] = newReplicas

	s.store.SetScale(name, updated)

	// Fire container start/stop in the background if replicas crossed 0.
	if newReplicas != oldReplicas && s.docker != nil {
		mapName := updated.Metadata.Annotations["igw.funcom.com/map-name"]
		if mapName != "" {
			service := "game-server-" + safeMapName(mapName)
			go s.applyScale(service, oldReplicas, newReplicas, name)
		}
	}

	log.Printf("[%s] PATCH serversetscales/%s — replicas=%d→%d (%s)",
		ns, name, oldReplicas, newReplicas, kind)
	writeJSON(w, http.StatusOK, updated)
}

// ---- Node + Pod (core K8s API) ----

// getNode answers run.sh's external-IP probe with the orchestrator's
// configured HOST_IP. K8s populates these from kubelet; we just hand back
// what we know.
func (s *Server) getNode(w http.ResponseWriter, r *http.Request, name string) {
	ip := s.hostIP
	if ip == "" {
		ip = "127.0.0.1"
	}
	node := Node{
		APIVersion: "v1",
		Kind:       "Node",
		Metadata: Metadata{
			Name:            name,
			ResourceVersion: s.store.ListResourceVersion(),
		},
		Status: NodeStatus{
			Addresses: []NodeAddress{
				{Type: "InternalIP", Address: ip},
				{Type: "ExternalIP", Address: ip},
				{Type: "Hostname", Address: name},
			},
		},
	}
	logGET(r, "nodes", name)
	writeJSON(w, http.StatusOK, node)
}

func (s *Server) getPod(w http.ResponseWriter, r *http.Request, ns, name string) {
	p := s.store.GetOrCreatePod(ns, name)
	logGET(r, "pods", name)
	writeJSON(w, http.StatusOK, p)
}

// patchPodStatus accepts run.sh's port/PID annotation PATCH on either
// /pods/{name} or /pods/{name}/status. K8s splits the two URLs to gate which
// fields are mutable; we don't enforce that distinction — the only client is
// run.sh and we just want to ack and store whatever it sends.
func (s *Server) patchPodStatus(w http.ResponseWriter, r *http.Request, ns, name string, body []byte) {
	p := s.store.GetOrCreatePod(ns, name)

	patch, err := parsePatchBody(body)
	if err != nil {
		log.Printf("[%s] PATCH pods/%s: bad body: %v", ns, name, err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	current, err := jsonRoundTrip(p)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	merged, ok := applyMergePatch(current, patch).(map[string]interface{})
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	updated, err := mapToPod(merged)
	if err != nil {
		log.Printf("[%s] PATCH pods/%s: rebuild: %v", ns, name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.store.SetPod(name, updated)

	log.Printf("[%s] PATCH pods/%s — annotations=%v", ns, name, updated.Metadata.Annotations)
	writeJSON(w, http.StatusOK, updated)
}

func mapToPod(m map[string]interface{}) (*Pod, error) {
	p := &Pod{
		APIVersion: getString(m, "apiVersion"),
		Kind:       getString(m, "kind"),
		Spec:       getMap(m, "spec"),
		Status:     getMap(m, "status"),
	}
	if md := getMap(m, "metadata"); md != nil {
		p.Metadata = Metadata{
			Name:        getString(md, "name"),
			Namespace:   getString(md, "namespace"),
			Labels:      getStringMap(md, "labels"),
			Annotations: getStringMap(md, "annotations"),
		}
	}
	if p.APIVersion == "" {
		p.APIVersion = "v1"
	}
	if p.Kind == "" {
		p.Kind = "Pod"
	}
	if p.Metadata.Name == "" {
		return nil, fmt.Errorf("merged result missing metadata.name")
	}
	if p.Status == nil {
		p.Status = map[string]interface{}{}
	}
	return p, nil
}

// applyScale drives docker start/stop in response to a ServerSetScale PATCH.
// Runs in a goroutine because container start can take tens of seconds and we
// don't want to block the director's HTTP request. Errors are logged but not
// propagated; the orchestrator will catch up on the next director poll if
// docker eventually recovers.
func (s *Server) applyScale(service string, old, new int, scaleName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	switch {
	case old == 0 && new >= 1:
		log.Printf("docker start %s (scale %s: %d→%d)", service, scaleName, old, new)
		if err := s.docker.Start(ctx, service); err != nil {
			log.Printf("docker start %s failed: %v", service, err)
		}
	case old >= 1 && new == 0:
		log.Printf("docker stop %s (scale %s: %d→%d)", service, scaleName, old, new)
		if err := s.docker.Stop(ctx, service, 120); err != nil {
			log.Printf("docker stop %s failed: %v", service, err)
		}
	}
}

// ---- helpers ----

func parsePatchBody(body []byte) (map[string]interface{}, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return map[string]interface{}{}, nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, resource, name string) {
	log.Printf("404 %s %s (resource=%s name=%s)", r.Method, r.URL.Path, resource, name)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"message":    resource + " \"" + name + "\" not found",
		"reason":     "NotFound",
		"code":       404,
	})
}

func (s *Server) unhandled(w http.ResponseWriter, r *http.Request, body string) {
	if body != "" {
		log.Printf("UNHANDLED %s %s — body: %s", r.Method, r.URL.Path, body)
	} else {
		log.Printf("UNHANDLED %s %s", r.Method, r.URL.Path)
	}
	w.WriteHeader(http.StatusNotFound)
}

func logGET(r *http.Request, resource, name string) {
	log.Printf("GET %s/%s → 200", resource, name)
}

// mapToBattleGroup rebuilds a typed BattleGroup from a generic map after a
// merge patch. We only extract the fields we model; everything else lives in
// .Spec/.Status as map[string]interface{} so it survives round-trips.
func mapToBattleGroup(m map[string]interface{}) (*BattleGroup, error) {
	bg := &BattleGroup{
		APIVersion: getString(m, "apiVersion"),
		Kind:       getString(m, "kind"),
		Spec:       getMap(m, "spec"),
		Status:     getMap(m, "status"),
	}
	if md := getMap(m, "metadata"); md != nil {
		bg.Metadata = Metadata{
			Name:        getString(md, "name"),
			Namespace:   getString(md, "namespace"),
			Labels:      getStringMap(md, "labels"),
			Annotations: getStringMap(md, "annotations"),
		}
	}
	if bg.APIVersion == "" || bg.Kind == "" || bg.Metadata.Name == "" {
		return nil, fmt.Errorf("merged result missing apiVersion/kind/metadata.name")
	}
	return bg, nil
}

func mapToScale(m map[string]interface{}) (*ServerSetScale, error) {
	sc := &ServerSetScale{
		APIVersion: getString(m, "apiVersion"),
		Kind:       getString(m, "kind"),
		Spec:       getMap(m, "spec"),
		Status:     getMap(m, "status"),
	}
	if md := getMap(m, "metadata"); md != nil {
		sc.Metadata = Metadata{
			Name:        getString(md, "name"),
			Namespace:   getString(md, "namespace"),
			Labels:      getStringMap(md, "labels"),
			Annotations: getStringMap(md, "annotations"),
		}
	}
	if sc.APIVersion == "" || sc.Kind == "" || sc.Metadata.Name == "" {
		return nil, fmt.Errorf("merged result missing apiVersion/kind/metadata.name")
	}
	return sc, nil
}

// summarize* are log-friendly one-liners. We log the full JSON only in
// debug-style — the volume of GETs is too high otherwise.

func summarizeServerStats(stats map[string]interface{}) string {
	status := getMap(stats, "status")
	if status == nil {
		return "empty"
	}
	runtime := getMap(status, "runtime")
	area := getMap(getMap(stats, "spec"), "area")
	idx := getInt(runtime, "serverIndex", -1)
	phase := getString(runtime, "gamePhase")
	ready, _ := runtime["ready"].(bool)
	mapName := getString(area, "map")
	partition := getInt(area, "partition", -1)
	return fmt.Sprintf("serverIndex=%d ready=%v phase=%s map=%s partition=%d",
		idx, ready, phase, mapName, partition)
}

func summarizeDirectorStats(stats map[string]interface{}) string {
	status := getMap(stats, "status")
	if status == nil {
		return "empty"
	}
	srvStates := getMap(status, "serverStates")
	return fmt.Sprintf("activityRatio=%v servers=%d",
		status["activityRatio"], len(srvStates))
}
