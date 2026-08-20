package config

import (
	"sort"
	"sync/atomic"
)

// DiscoveredModel describes one model discovered at runtime from a peer's
// own OpenAI-compatible /v1/models-shaped endpoint (see PeerDiscoveryConfig).
type DiscoveredModel struct {
	PeerID       string
	ModelID      string
	Name         string
	Capabilities ModelCapConfig
}

// peerModelsView is an immutable snapshot of every currently discovered
// peer model, indexed two ways: by peer (so refreshing one peer replaces
// exactly that peer's set) and flattened by fully qualified name (for O(1)
// resolution on the request path). Both indexes are rebuilt together and
// published as a single atomic swap, so a reader never observes one index
// updated and the other stale.
type peerModelsView struct {
	byPeer map[string]map[string]DiscoveredModel
	byFQN  map[string]DiscoveredModel
}

// PeerRegistry holds the current set of discovered peer models. Discovered
// models are addressable only by their fully qualified "peerID/modelID"
// name - there is deliberately no "bare name" index here, so refreshing
// one peer can never change how a name belonging to a different peer
// resolves (unlike statically configured peer.models, whose bare-name
// aliasing is unchanged and lives entirely in internal/router/peer.go).
//
// Safe for concurrent use: every method is nil-receiver-safe, so a bare
// config.Config{} test literal (the vast majority of them in this
// codebase) works without allocating a registry. Writers (one per peer's
// discovery goroutine) update via a compare-and-swap retry loop; readers
// see a complete, consistent snapshot at all times.
type PeerRegistry struct {
	view atomic.Pointer[peerModelsView]
}

// NewPeerRegistry returns an empty, ready-to-use registry.
func NewPeerRegistry() *PeerRegistry {
	r := &PeerRegistry{}
	r.view.Store(&peerModelsView{
		byPeer: map[string]map[string]DiscoveredModel{},
		byFQN:  map[string]DiscoveredModel{},
	})
	return r
}

// SetPeerModels replaces the discovered model set for one peer. An empty
// or nil map clears that peer's discovered models entirely. Callers that
// want to keep the previous set after a failed refresh should simply not
// call this rather than passing the old set back in.
func (r *PeerRegistry) SetPeerModels(peerID string, models map[string]DiscoveredModel) {
	if r == nil {
		return
	}
	for {
		oldPtr := r.view.Load()
		var old peerModelsView
		if oldPtr != nil {
			old = *oldPtr
		}

		nextByPeer := make(map[string]map[string]DiscoveredModel, len(old.byPeer)+1)
		for id, m := range old.byPeer {
			if id == peerID {
				continue
			}
			nextByPeer[id] = m
		}
		if len(models) > 0 {
			nextByPeer[peerID] = models
		}

		nextByFQN := make(map[string]DiscoveredModel, len(nextByPeer)*8)
		for id, m := range nextByPeer {
			for modelID, dm := range m {
				nextByFQN[PeerModelFQN(id, modelID)] = dm
			}
		}

		next := &peerModelsView{byPeer: nextByPeer, byFQN: nextByFQN}
		if r.view.CompareAndSwap(oldPtr, next) {
			return
		}
	}
}

// LookupFQN returns the discovered model registered under an exact fully
// qualified name, if any.
func (r *PeerRegistry) LookupFQN(fqn string) (DiscoveredModel, bool) {
	if r == nil {
		return DiscoveredModel{}, false
	}
	view := r.view.Load()
	if view == nil {
		return DiscoveredModel{}, false
	}
	dm, ok := view.byFQN[fqn]
	return dm, ok
}

// Models returns every currently discovered model across all peers, sorted
// by peer ID then model ID for deterministic iteration (used by
// /v1/models and /api/events).
func (r *PeerRegistry) Models() []DiscoveredModel {
	if r == nil {
		return nil
	}
	view := r.view.Load()
	if view == nil {
		return nil
	}

	peerIDs := make([]string, 0, len(view.byPeer))
	for id := range view.byPeer {
		peerIDs = append(peerIDs, id)
	}
	sort.Strings(peerIDs)

	all := make([]DiscoveredModel, 0, len(view.byFQN))
	for _, id := range peerIDs {
		models := view.byPeer[id]
		modelIDs := make([]string, 0, len(models))
		for modelID := range models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			all = append(all, models[modelID])
		}
	}
	return all
}
