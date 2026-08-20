package config

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPeerRegistry_NilSafe(t *testing.T) {
	var r *PeerRegistry

	// None of these should panic on a nil receiver - config.Config{} test
	// literals throughout the codebase never allocate PeerModels.
	require.NotPanics(t, func() {
		r.SetPeerModels("peer1", map[string]DiscoveredModel{"m": {}})
	})

	_, found := r.LookupFQN("peer1/m")
	assert.False(t, found)

	assert.Empty(t, r.Models())
}

func TestPeerRegistry_EmptyByDefault(t *testing.T) {
	r := NewPeerRegistry()
	assert.Empty(t, r.Models())
	_, found := r.LookupFQN("peer1/model")
	assert.False(t, found)
}

func TestPeerRegistry_SetAndLookupFQN(t *testing.T) {
	r := NewPeerRegistry()
	r.SetPeerModels("openrouter", map[string]DiscoveredModel{
		"z-ai/glm-4.7": {
			PeerID:  "openrouter",
			ModelID: "z-ai/glm-4.7",
			Name:    "GLM 4.7",
		},
	})

	dm, found := r.LookupFQN("openrouter/z-ai/glm-4.7")
	require.True(t, found)
	assert.Equal(t, "openrouter", dm.PeerID)
	assert.Equal(t, "z-ai/glm-4.7", dm.ModelID)
	assert.Equal(t, "GLM 4.7", dm.Name)

	_, found = r.LookupFQN("openrouter/unknown-model")
	assert.False(t, found)
}

func TestPeerRegistry_SetReplacesOnlyThatPeer(t *testing.T) {
	r := NewPeerRegistry()
	r.SetPeerModels("a", map[string]DiscoveredModel{"m1": {PeerID: "a", ModelID: "m1"}})
	r.SetPeerModels("b", map[string]DiscoveredModel{"m2": {PeerID: "b", ModelID: "m2"}})

	// Refreshing peer "a" must not disturb peer "b"'s entries.
	r.SetPeerModels("a", map[string]DiscoveredModel{"m1-new": {PeerID: "a", ModelID: "m1-new"}})

	_, found := r.LookupFQN("a/m1")
	assert.False(t, found, "stale entry from peer a's previous refresh should be gone")
	_, found = r.LookupFQN("a/m1-new")
	assert.True(t, found)
	_, found = r.LookupFQN("b/m2")
	assert.True(t, found, "peer b's entries must survive peer a's refresh")
}

func TestPeerRegistry_SetEmptyClearsPeer(t *testing.T) {
	r := NewPeerRegistry()
	r.SetPeerModels("a", map[string]DiscoveredModel{"m1": {PeerID: "a", ModelID: "m1"}})
	r.SetPeerModels("a", map[string]DiscoveredModel{})

	_, found := r.LookupFQN("a/m1")
	assert.False(t, found)
	assert.Empty(t, r.Models())
}

func TestPeerRegistry_ModelsSortedByPeerThenModel(t *testing.T) {
	r := NewPeerRegistry()
	r.SetPeerModels("b-peer", map[string]DiscoveredModel{
		"z-model": {PeerID: "b-peer", ModelID: "z-model"},
		"a-model": {PeerID: "b-peer", ModelID: "a-model"},
	})
	r.SetPeerModels("a-peer", map[string]DiscoveredModel{
		"only": {PeerID: "a-peer", ModelID: "only"},
	})

	models := r.Models()
	require.Len(t, models, 3)
	assert.Equal(t, "a-peer", models[0].PeerID)
	assert.Equal(t, "b-peer", models[1].PeerID)
	assert.Equal(t, "a-model", models[1].ModelID)
	assert.Equal(t, "b-peer", models[2].PeerID)
	assert.Equal(t, "z-model", models[2].ModelID)
}

// TestPeerRegistry_ConcurrentPeersDoNotRace exercises the CAS retry loop:
// multiple peers refreshing concurrently must not lose updates or corrupt
// the shared index. Run with -race.
func TestPeerRegistry_ConcurrentPeersDoNotRace(t *testing.T) {
	r := NewPeerRegistry()
	const peers = 8
	const rounds = 50

	var wg sync.WaitGroup
	for p := 0; p < peers; p++ {
		wg.Add(1)
		go func(peerIdx int) {
			defer wg.Done()
			peerID := string(rune('a' + peerIdx))
			for i := 0; i < rounds; i++ {
				r.SetPeerModels(peerID, map[string]DiscoveredModel{
					"m": {PeerID: peerID, ModelID: "m"},
				})
				r.LookupFQN(peerID + "/m")
				r.Models()
			}
		}(p)
	}
	wg.Wait()

	models := r.Models()
	assert.Len(t, models, peers, "every peer should have exactly one surviving entry")
}
