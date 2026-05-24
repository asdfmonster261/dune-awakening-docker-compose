package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Metadata is the K8s object metadata subset we care about.
// Labels and Annotations are present for ServerSetScale because the bg-director
// derives the map identity from them; the real K8s operator emits them on
// every ServerSetScale it creates.
type Metadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// BattleGroup is the master CR. We keep spec/status as untyped maps so we can
// serve back whatever shape the K8s setup expects without knowing every field.
// Status is non-omitempty because the bg-director's C# deserializer marks it
// [JsonRequired] and rejects the CR if the key is absent.
type BattleGroup struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   Metadata               `json:"metadata"`
	Spec       map[string]interface{} `json:"spec,omitempty"`
	Status     map[string]interface{} `json:"status"`
}

// ServerSetScale is one entry of the serversetscales subresource list.
type ServerSetScale struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   Metadata               `json:"metadata"`
	Spec       map[string]interface{} `json:"spec,omitempty"`
	Status     map[string]interface{} `json:"status,omitempty"`
}

// ServerSetScaleList is what GET /serversetscales returns.
type ServerSetScaleList struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Metadata   ListMetadata     `json:"metadata"`
	Items      []ServerSetScale `json:"items"`
}

type ListMetadata struct {
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// Pod is the minimal K8s Pod we round-trip for run.sh. On startup, run.sh
// PATCHes /pods/{name}/status with metadata.annotations carrying gamePort,
// igwPort, gamePid, sshPort. We store the merged result so subsequent GETs
// (from any client that wants to read those annotations) return the latest
// state.
type Pod struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Metadata   Metadata               `json:"metadata"`
	Spec       map[string]interface{} `json:"spec,omitempty"`
	Status     map[string]interface{} `json:"status"`
}

// Node is the cluster-scoped object run.sh GETs to learn its external IP.
// fetch_external_node_address reads status.addresses[]; if the response
// carries a usable address it skips the POD_IP fallback. We always answer
// with the orchestrator's configured HOST_IP.
type Node struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   Metadata   `json:"metadata"`
	Status     NodeStatus `json:"status"`
}

type NodeStatus struct {
	Addresses []NodeAddress `json:"addresses"`
}

type NodeAddress struct {
	Type    string `json:"type"`
	Address string `json:"address"`
}

// Store holds all the in-memory state. No persistence — restart reloads from
// world.yaml; running game-servers/director re-PATCH within seconds.
type Store struct {
	mu             sync.RWMutex
	battleGroup    *BattleGroup
	scales         map[string]*ServerSetScale
	serverStats    map[string]map[string]interface{}
	directorStats  map[string]map[string]interface{}
	pods           map[string]*Pod
	resourceVerCtr atomic.Uint64
}

func NewStore() *Store {
	return &Store{
		scales:        make(map[string]*ServerSetScale),
		serverStats:   make(map[string]map[string]interface{}),
		directorStats: make(map[string]map[string]interface{}),
		pods:          make(map[string]*Pod),
	}
}

func (s *Store) nextResourceVersion() string {
	return fmt.Sprintf("%d", s.resourceVerCtr.Add(1))
}

// --- BattleGroup ---

func (s *Store) GetBattleGroup() *BattleGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.battleGroup
}

func (s *Store) SetBattleGroup(bg *BattleGroup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bg.Metadata.ResourceVersion = s.nextResourceVersion()
	s.battleGroup = bg
}

// --- ServerSetScale ---

func (s *Store) ListScales() []ServerSetScale {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ServerSetScale, 0, len(s.scales))
	for _, sc := range s.scales {
		out = append(out, *sc)
	}
	return out
}

func (s *Store) GetScale(name string) *ServerSetScale {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sc, ok := s.scales[name]; ok {
		copy := *sc
		return &copy
	}
	return nil
}

func (s *Store) SetScale(name string, sc *ServerSetScale) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc.Metadata.ResourceVersion = s.nextResourceVersion()
	s.scales[name] = sc
}

// --- Stats (PATCH targets we just store and acknowledge) ---

func (s *Store) SetServerStats(name string, stats map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serverStats[name] = stats
}

func (s *Store) GetServerStats(name string) map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if stats, ok := s.serverStats[name]; ok {
		return stats
	}
	return nil
}

// ListServerStats returns a snapshot of every server's most recently
// PATCHed stats. Each entry's metadata.name is the K8s-style server name
// (e.g. "game-server-sh-arrakeen-0"); callers can correlate to the
// matching ServerSetScale via the same prefix.
func (s *Store) ListServerStats() []map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(s.serverStats))
	for _, stats := range s.serverStats {
		out = append(out, stats)
	}
	return out
}

func (s *Store) SetDirectorStats(name string, stats map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.directorStats[name] = stats
}

func (s *Store) ListResourceVersion() string {
	return s.nextResourceVersion()
}

// --- Pods ---

// GetOrCreatePod returns the existing Pod or a bare one with the supplied
// identity. Auto-vivifying on read means a GET that races ahead of run.sh's
// first PATCH doesn't 404 — it just returns an empty pod.
func (s *Store) GetOrCreatePod(ns, name string) *Pod {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pods[name]; ok {
		cp := *p
		return &cp
	}
	p := &Pod{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: Metadata{
			Name:            name,
			Namespace:       ns,
			ResourceVersion: s.nextResourceVersion(),
		},
		Spec:   map[string]interface{}{},
		Status: map[string]interface{}{},
	}
	s.pods[name] = p
	cp := *p
	return &cp
}

func (s *Store) SetPod(name string, p *Pod) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.Metadata.ResourceVersion = s.nextResourceVersion()
	s.pods[name] = p
}
