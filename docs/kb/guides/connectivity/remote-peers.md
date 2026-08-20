---
title: Route to remote peers
summary: Expose models from another llama-swap host through a peer connection, by hand or auto-discovered.
category: guides
tags: [peers, remote, networking, discovery]
config_keys: [peers, peers.*.proxy, peers.*.apiKey, peers.*.models, peers.*.discovery]
updated: 2026-09-01
---

# Route to remote peers

Declare a peer with the other llama-swap `proxy` URL and its model list. Its models are addressed as
`peer-name/model-id`, so a peer named `sippy` serving `gemma-4-12B` appears as
`sippy/gemma-4-12B`.

```yaml
peers:
  sippy:
    proxy: http://sippy:8080
    apiKey: ${env.SIPPY_API_KEY}
    models: [gemma-4-12B]
```

The peer must be reachable from this host and its API key must match the remote
server. See `guides/api-integration/api-keys-and-auth` for keeping it out of
the committed file.

## Auto-discovering a peer's models

Instead of (or alongside) listing every model under `models:` by hand, add a `discovery:`
block and llama-swap polls the peer's own `/v1/models` and keeps the list in sync:

```yaml
peers:
  openrouter:
    proxy: https://openrouter.ai/api
    apiKey: ${env.OPENROUTER_API_KEY}
    discovery:
      refreshInterval: 300
      exclude: ["*:free"]
```

Discovery works against any server that accepts the same `Authorization: Bearer <apiKey>`
auth this peer connection already sends and answers an OpenAI-compatible `/v1/models` -
OpenAI, xAI, Mistral, DeepSeek, Cerebras, Together, vLLM, LiteLLM, Ollama, OpenRouter, and
another llama-swap all work out of the box. Anthropic and Gemini's native API use a
different response shape and auth and are not supported.

A failed fetch (peer unreachable, DNS not ready yet) is retried with a short backoff
(`retryInterval`, doubling up to `refreshInterval`) rather than waiting for the next full
`refreshInterval` - useful when that interval is set high. Models found only through
discovery are addressable exclusively by their full `peer-name/model-id` name; they don't
get a bare alias and can't be targeted by `selectors` or `profiles`. Models listed under
`models:` keep working alongside discovery and are unaffected by `include`/`exclude`.
