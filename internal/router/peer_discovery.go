package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// maxDiscoveryResponseBytes bounds how much of a peer's /v1/models response
// this reads into memory. A few hundred KB covers OpenRouter's full catalog
// (hundreds of models); this caps a misbehaving or malicious peer from
// forcing unbounded memory use.
const maxDiscoveryResponseBytes = 10 << 20 // 10 MiB

// validDiscoveryModalities mirrors config.ModelCapConfig's accepted
// modality vocabulary. A peer reporting a modality outside this set (e.g.
// OpenRouter's "file") causes the whole capabilities block for that model
// to be dropped rather than emitting a partial, misleading result.
var validDiscoveryModalities = map[string]bool{"text": true, "audio": true, "image": true}

// discoveryModel is the union of /v1/models item fields this parser
// understands across OpenAI, OpenRouter, Mistral, and LiteLLM-shaped
// responses. Any field a given peer doesn't send simply decodes to its zero
// value.
type discoveryModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	Architecture *struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`

	// Capabilities covers both the object llama-swap/OpenRouter render
	// under "capabilities" (vision, audio_transcriptions, audio_speech,
	// image_generation, image_to_image, function_calling, reranker) and
	// Mistral's own "capabilities" object, which happens to share the
	// vision/function_calling field names.
	Capabilities *struct {
		Vision              bool `json:"vision"`
		AudioTranscriptions bool `json:"audio_transcriptions"`
		AudioSpeech         bool `json:"audio_speech"`
		ImageGeneration     bool `json:"image_generation"`
		ImageToImage        bool `json:"image_to_image"`
		FunctionCalling     bool `json:"function_calling"`
		Reranker            bool `json:"reranker"`
	} `json:"capabilities"`

	SupportedParameters []string `json:"supported_parameters"`
	ContextLength       int      `json:"context_length"`

	// MaxInputTokens is LiteLLM's (and Anthropic's) name for what
	// OpenRouter calls context_length. Many LiteLLM proxy deployments
	// enrich their otherwise plain OpenAI-shaped /v1/models with this
	// field but never send context_length, architecture, or a
	// capabilities object - confirmed against a live proxy, where the
	// full item shape was just
	// {"id","object","created","owned_by","max_input_tokens","max_output_tokens"}.
	MaxInputTokens int `json:"max_input_tokens"`

	Meta *struct {
		NCtx int `json:"n_ctx"`
	} `json:"meta"`
}

// FetchDiscoveredModels performs one discovery request against a peer's
// discovery.path and returns the filtered, capability-mapped model set
// keyed by model ID.
//
// httpClient should be the peer's own configured client/transport so
// auth/TLS/proxy behaviour matches what ServeHTTP would use for normal
// proxied requests. apiKey is injected the same way ServeHTTP injects it
// (Authorization: Bearer + x-api-key) - see peer.go's ServeHTTP.
//
// This is intentionally limited to OpenAI-compatible {"data":[...]}-shaped
// responses (see docs/configuration.md). Any other shape - e.g. Anthropic's
// distinct auth/header requirements and response fields, Gemini native's
// {"models":[...]} envelope, or a bare JSON array (Together AI) - returns
// an error rather than silently misinterpreting the payload; the caller is
// expected to log it as a warning and keep the peer's previously known
// model set (see internal/router's discovery poll loop).
//
// ctx must carry a deadline: some peers configure "responseHeader: 0" (no
// timeout) on their own transport settings, so relying on the transport
// alone could pin the calling goroutine indefinitely on a hanging peer.
func FetchDiscoveredModels(
	ctx context.Context,
	httpClient *http.Client,
	peerID string,
	baseURL string,
	discovery config.PeerDiscoveryConfig,
	apiKey string,
) (map[string]config.DiscoveredModel, error) {
	url := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(discovery.Path, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building discovery request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discovery request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("reading discovery response from %s: %w", url, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery request to %s returned status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Decode into a raw envelope first so "no top-level 'data' key at all"
	// (Gemini native's {"models":[...]}, or a bare array) can be
	// distinguished from "data": [] (a recognized, legitimately empty
	// response).
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding discovery response from %s: %w", url, err)
	}
	rawData, ok := envelope["data"]
	if !ok {
		return nil, fmt.Errorf("unrecognized /v1/models response shape from %s: no top-level \"data\" array", url)
	}

	var items []discoveryModel
	if err := json.Unmarshal(rawData, &items); err != nil {
		return nil, fmt.Errorf("decoding discovery response \"data\" array from %s: %w", url, err)
	}

	models := make(map[string]config.DiscoveredModel, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if !config.GlobsMatch(id, discovery.IncludeRe, discovery.ExcludeRe) {
			continue
		}

		var caps config.ModelCapConfig
		if discovery.Capabilities {
			caps = mapDiscoveryCapabilities(item)
		}

		models[id] = config.DiscoveredModel{
			PeerID:       peerID,
			ModelID:      id,
			Name:         strings.TrimSpace(item.Name),
			Capabilities: caps,
		}
	}

	return models, nil
}

// mapDiscoveryCapabilities is the inverse of internal/server's
// renderCapabilities: given one discovery response item, it produces the
// config.ModelCapConfig llama-swap would need to render the same
// architecture/capabilities/context_length fields back out through its own
// /v1/models.
func mapDiscoveryCapabilities(item discoveryModel) config.ModelCapConfig {
	var caps config.ModelCapConfig

	switch {
	case item.Architecture != nil && (len(item.Architecture.InputModalities) > 0 || len(item.Architecture.OutputModalities) > 0):
		in, inOK := sanitizeModalities(item.Architecture.InputModalities)
		out, outOK := sanitizeModalities(item.Architecture.OutputModalities)
		if !inOK || !outOK {
			// An unrecognized modality (e.g. OpenRouter's "file") makes any
			// partial In/Out list actively misleading - renderCapabilities
			// would compose a bogus "modality" string from it. Drop the
			// whole capabilities block for this model instead.
			return config.ModelCapConfig{}
		}
		caps.In = in
		caps.Out = out
	case item.Capabilities != nil:
		if item.Capabilities.Vision || item.Capabilities.ImageToImage {
			caps.In = appendUniqueModality(caps.In, "image")
		}
		if item.Capabilities.AudioTranscriptions {
			caps.In = appendUniqueModality(caps.In, "audio")
			caps.Out = appendUniqueModality(caps.Out, "text")
		}
		if item.Capabilities.AudioSpeech {
			caps.In = appendUniqueModality(caps.In, "text")
			caps.Out = appendUniqueModality(caps.Out, "audio")
		}
		if item.Capabilities.ImageGeneration || item.Capabilities.ImageToImage {
			caps.In = appendUniqueModality(caps.In, "text")
			caps.Out = appendUniqueModality(caps.Out, "image")
		}
	}

	if item.Capabilities != nil && item.Capabilities.FunctionCalling {
		caps.Tools = true
	}
	if !caps.Tools {
		for _, p := range item.SupportedParameters {
			if p == "tools" {
				caps.Tools = true
				break
			}
		}
	}
	if item.Capabilities != nil && item.Capabilities.Reranker {
		caps.Reranker = true
	}

	switch {
	case item.ContextLength > 0:
		caps.Context = item.ContextLength
	case item.Meta != nil && item.Meta.NCtx > 0:
		caps.Context = item.Meta.NCtx
	case item.MaxInputTokens > 0:
		caps.Context = item.MaxInputTokens
	}

	return caps
}

// sanitizeModalities returns mods unchanged (ok=true) if every entry is one
// of config.ModelCapConfig's accepted modalities, or (nil, false) if any
// entry is not.
func sanitizeModalities(mods []string) (out []string, ok bool) {
	out = make([]string, 0, len(mods))
	for _, m := range mods {
		if !validDiscoveryModalities[m] {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

func appendUniqueModality(modalities []string, m string) []string {
	for _, existing := range modalities {
		if existing == m {
			return modalities
		}
	}
	return append(modalities, m)
}
