package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func defaultDiscoveryConfig(t *testing.T) config.PeerDiscoveryConfig {
	t.Helper()
	return config.DefaultPeerDiscoveryConfig()
}

func TestFetchDiscoveredModels_OpenAIShapedEnvelope(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-4o","object":"model","created":1,"owned_by":"openai"},
			{"id":"gpt-4o-mini","object":"model","created":1,"owned_by":"openai"}
		]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "openai", testServer.URL, discovery, "")
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "openai", models["gpt-4o"].PeerID)
	assert.Equal(t, "gpt-4o", models["gpt-4o"].ModelID)
	assert.Contains(t, models, "gpt-4o-mini")
}

func TestFetchDiscoveredModels_OpenRouterShapedEnvelope(t *testing.T) {
	// OpenRouter omits "object":"list" and adds total_count/links, but still
	// keys the array under "data" - the parser must not require the
	// "object" field to be present.
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[
			{"id":"z-ai/glm-4.7","name":"GLM 4.7","context_length":200000,
			 "architecture":{"input_modalities":["text"],"output_modalities":["text"]},
			 "supported_parameters":["tools","temperature"]}
		],"total_count":1}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "openrouter", testServer.URL, discovery, "")
	require.NoError(t, err)
	require.Contains(t, models, "z-ai/glm-4.7")

	dm := models["z-ai/glm-4.7"]
	assert.Equal(t, "GLM 4.7", dm.Name)
	assert.Equal(t, []string{"text"}, dm.Capabilities.In)
	assert.Equal(t, []string{"text"}, dm.Capabilities.Out)
	assert.True(t, dm.Capabilities.Tools)
	assert.Equal(t, 200000, dm.Capabilities.Context)
}

func TestFetchDiscoveredModels_MistralCapabilitiesObject(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[
			{"id":"pixtral-large-latest","capabilities":{"vision":true,"function_calling":true},
			 "max_context_length":128000}
		]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "mistral", testServer.URL, discovery, "")
	require.NoError(t, err)
	require.Contains(t, models, "pixtral-large-latest")

	dm := models["pixtral-large-latest"]
	assert.Contains(t, dm.Capabilities.In, "image")
	assert.True(t, dm.Capabilities.Tools)
}

func TestFetchDiscoveredModels_InjectsAuthHeaders(t *testing.T) {
	var gotAuth, gotAPIKey string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	_, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "peer1", testServer.URL, discovery, "sk-test-key")
	require.NoError(t, err)
	assert.Equal(t, "Bearer sk-test-key", gotAuth)
	assert.Equal(t, "sk-test-key", gotAPIKey)
}

func TestFetchDiscoveredModels_CustomPath(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/openai/v1/models", r.URL.Path)
		w.Write([]byte(`{"object":"list","data":[{"id":"llama-3.3-70b-versatile"}]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	discovery.Path = "/openai/v1/models"
	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "groq", testServer.URL, discovery, "")
	require.NoError(t, err)
	assert.Contains(t, models, "llama-3.3-70b-versatile")
}

func TestFetchDiscoveredModels_IncludeExcludeFiltering(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[
			{"id":"openai/gpt-4o"},
			{"id":"openai/gpt-4o:free"},
			{"id":"anthropic/claude"}
		]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	discovery.IncludeRe, _ = config.CompileGlobs([]string{"openai/*"})
	discovery.ExcludeRe, _ = config.CompileGlobs([]string{"*:free"})

	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "peer1", testServer.URL, discovery, "")
	require.NoError(t, err)
	assert.Contains(t, models, "openai/gpt-4o")
	assert.NotContains(t, models, "openai/gpt-4o:free")
	assert.NotContains(t, models, "anthropic/claude")
}

func TestFetchDiscoveredModels_UnrecognizedModalityDropsCapsButKeepsModel(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OpenRouter's "file" modality is outside {text,audio,image}.
		w.Write([]byte(`{"object":"list","data":[
			{"id":"some-model","architecture":{"input_modalities":["text","file"],"output_modalities":["text"]}}
		]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "peer1", testServer.URL, discovery, "")
	require.NoError(t, err)
	require.Contains(t, models, "some-model")
	assert.Empty(t, models["some-model"].Capabilities.In)
	assert.Empty(t, models["some-model"].Capabilities.Out)
}

func TestFetchDiscoveredModels_CapabilitiesDisabled(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[
			{"id":"some-model","context_length":32000,
			 "architecture":{"input_modalities":["text"],"output_modalities":["text"]}}
		]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	discovery.Capabilities = false
	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "peer1", testServer.URL, discovery, "")
	require.NoError(t, err)
	assert.Equal(t, 0, models["some-model"].Capabilities.Context)
	assert.Empty(t, models["some-model"].Capabilities.In)
}

func TestFetchDiscoveredModels_EmptyDataArrayIsNotAnError(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	models, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "peer1", testServer.URL, discovery, "")
	require.NoError(t, err)
	assert.Empty(t, models)
}

func TestFetchDiscoveredModels_NonOKStatusReturnsError(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal error`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	_, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "peer1", testServer.URL, discovery, "")
	require.Error(t, err)
}

func TestFetchDiscoveredModels_UnreachablePeerReturnsError(t *testing.T) {
	discovery := defaultDiscoveryConfig(t)
	_, err := FetchDiscoveredModels(context.Background(), http.DefaultClient, "peer1", "http://127.0.0.1:1", discovery, "")
	require.Error(t, err)
}

// TestFetchDiscoveredModels_UnrecognizedShapeReturnsError covers responses
// that are valid JSON but not one of the supported OpenAI-compatible
// envelopes - e.g. Gemini native's {"models":[...]} or a bare array like
// Together AI's. These are explicitly out of scope for this feature (see
// AGENTS.md-adjacent design notes); discovery must degrade to "zero models,
// log a warning" rather than silently misinterpreting the payload or
// panicking.
func TestFetchDiscoveredModels_UnrecognizedShapeReturnsError(t *testing.T) {
	tests := map[string]string{
		"gemini-native models key": `{"models":[{"name":"models/gemini-2.5-pro"}]}`,
		"bare array (Together AI)": `[{"id":"meta-llama/Llama-3.3-70B"}]`,
		"not json at all":          `not json`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			}))
			defer testServer.Close()

			discovery := defaultDiscoveryConfig(t)
			_, err := FetchDiscoveredModels(context.Background(), testServer.Client(), "peer1", testServer.URL, discovery, "")
			assert.Error(t, err)
		})
	}
}

func TestFetchDiscoveredModels_ContextTimeout(t *testing.T) {
	started := make(chan struct{})
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer testServer.Close()

	discovery := defaultDiscoveryConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := FetchDiscoveredModels(ctx, testServer.Client(), "peer1", testServer.URL, discovery, "")
	require.Error(t, err)
	<-started
}
