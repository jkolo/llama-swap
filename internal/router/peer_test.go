package router

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/stretchr/testify/require"
	"github.com/tailscale/tailcat"
)

var testLogger = logmon.NewWriter(os.Stdout)

func init() {
	testLogger.SetLogLevel(logmon.LevelWarn)
}

func TestNewPeer_EmptyPeers(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil {
		t.Fatal("expected non-nil Peer")
	}
	if len(pr.currentRoutes()) != 0 {
		t.Fatalf("expected empty peers map, got %d entries", len(pr.currentRoutes()))
	}
}

func TestNewPeer_SinglePeer(t *testing.T) {
	proxyURL, _ := url.Parse("http://peer1.example.com:8080")
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    "http://peer1.example.com:8080",
			ProxyURL: proxyURL,
			ApiKey:   "test-key",
			Models:   []string{"model-a", "model-b"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.currentRoutes()) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(pr.currentRoutes()))
	}
	if _, ok := pr.currentRoutes()["model-a"]; !ok {
		t.Error("expected model-a to be mapped")
	}
	if _, ok := pr.currentRoutes()["model-b"]; !ok {
		t.Error("expected model-b to be mapped")
	}
	if _, ok := pr.currentRoutes()["peer1/model-a"]; !ok {
		t.Error("expected peer1/model-a to be mapped")
	}
	if _, ok := pr.currentRoutes()["peer1/model-b"]; !ok {
		t.Error("expected peer1/model-b to be mapped")
	}
	if _, ok := pr.currentRoutes()["model-c"]; ok {
		t.Error("expected model-c to not be mapped")
	}
}

func TestNewPeer_MultiplePeers(t *testing.T) {
	proxyURL1, _ := url.Parse("http://peer1.example.com:8080")
	proxyURL2, _ := url.Parse("http://peer2.example.com:8080")
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    "http://peer1.example.com:8080",
			ProxyURL: proxyURL1,
			Models:   []string{"model-a", "model-b"},
		},
		"peer2": config.PeerConfig{
			Proxy:    "http://peer2.example.com:8080",
			ProxyURL: proxyURL2,
			Models:   []string{"model-c", "model-d"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.currentRoutes()) != 8 {
		t.Fatalf("expected 8 entries, got %d", len(pr.currentRoutes()))
	}
	for _, m := range []string{"model-a", "model-b", "model-c", "model-d"} {
		if _, ok := pr.currentRoutes()[m]; !ok {
			t.Errorf("expected %s to be mapped", m)
		}
	}
	for _, m := range []string{"peer1/model-a", "peer1/model-b", "peer2/model-c", "peer2/model-d"} {
		if _, ok := pr.currentRoutes()[m]; !ok {
			t.Errorf("expected %s to be mapped", m)
		}
	}
}

func TestNewPeer_MembersIncludePeersWithoutModels(t *testing.T) {
	proxyURL, _ := url.Parse("http://peer.example.com:8080")
	peers := config.PeerDictionaryConfig{
		"modeled-peer": {
			ProxyURL: proxyURL,
			Models:   []string{"model-a"},
		},
		"empty-peer": {
			ProxyURL: proxyURL,
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(pr.members))
	}
	for _, peerID := range []string{"empty-peer", "modeled-peer"} {
		member, ok := pr.members[peerID]
		if !ok {
			t.Fatalf("peer %s has no member", peerID)
		}
		if member.peerID != peerID {
			t.Fatalf("members[%s].peerID = %s", peerID, member.peerID)
		}
	}
}

func TestNewPeer_DuplicateModel(t *testing.T) {
	proxyURL1, _ := url.Parse("http://peer1.example.com:8080")
	proxyURL2, _ := url.Parse("http://peer2.example.com:8080")
	peers := config.PeerDictionaryConfig{
		"alpha-peer": config.PeerConfig{
			Proxy:    "http://peer1.example.com:8080",
			ProxyURL: proxyURL1,
			Models:   []string{"duplicate-model"},
		},
		"beta-peer": config.PeerConfig{
			Proxy:    "http://peer2.example.com:8080",
			ProxyURL: proxyURL2,
			Models:   []string{"duplicate-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.currentRoutes()) != 2 {
		t.Fatalf("expected 2 qualified entries for duplicate model, got %d", len(pr.currentRoutes()))
	}
	if _, ok := pr.currentRoutes()["duplicate-model"]; ok {
		t.Error("duplicate bare model should not be mapped")
	}
	if _, ok := pr.currentRoutes()["alpha-peer/duplicate-model"]; !ok {
		t.Error("expected alpha-peer/duplicate-model to be mapped")
	}
	if _, ok := pr.currentRoutes()["beta-peer/duplicate-model"]; !ok {
		t.Error("expected beta-peer/duplicate-model to be mapped")
	}
}

func TestNewPeer_FQNPrecedesCollidingBareModel(t *testing.T) {
	proxyURL, _ := url.Parse("http://peer.example.com")
	pr, err := NewPeer(config.Config{Peers: config.PeerDictionaryConfig{
		"p1": {
			ProxyURL: proxyURL,
			Models:   []string{"model"},
		},
		"p2": {
			ProxyURL: proxyURL,
			Models:   []string{"p1/model"},
		},
	}}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	if got := pr.currentRoutes()["p1/model"]; got == nil || got.member.peerID != "p1" || got.modelID != "model" {
		t.Fatalf("p1/model route = %#v, want p1 model", got)
	}
	if got := pr.currentRoutes()["p2/p1/model"]; got == nil || got.member.peerID != "p2" || got.modelID != "p1/model" {
		t.Fatalf("p2/p1/model route = %#v, want p2 p1/model", got)
	}
}

func TestPeer_ServeHTTP_QualifiedModelRewritten(t *testing.T) {
	var upstreamModel string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := swaputil.ExtractModel(r)
		if err != nil {
			t.Errorf("ExtractModel: %v", err)
		} else {
			upstreamModel = data
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	pr, err := NewPeer(config.Config{Peers: config.PeerDictionaryConfig{
		"strix": {
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"Q3.6-27B-MTP"},
		},
	}}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"strix/Q3.6-27B-MTP","prompt":"hello"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamModel != "Q3.6-27B-MTP" {
		t.Fatalf("upstream model = %q, want Q3.6-27B-MTP", upstreamModel)
	}
}

func TestPeer_ServeHTTP_Success(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response from peer"))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "response from peer" {
		t.Errorf("expected 'response from peer', got %q", w.Body.String())
	}
}

func TestPeer_ServeHTTP_ModelNotFoundInContext(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeer_ServeHTTP_PeerModelNotFound(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "nonexistent-model", ModelID: "nonexistent-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeer_ServeHTTP_ApiKeyInjection(t *testing.T) {
	var receivedAuthHeader string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			ApiKey:   "secret-api-key",
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if receivedAuthHeader != "Bearer secret-api-key" {
		t.Errorf("expected 'Bearer secret-api-key', got %q", receivedAuthHeader)
	}
}

func TestPeer_ServeHTTP_NoApiKey(t *testing.T) {
	var receivedAuthHeader string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			ApiKey:   "",
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if receivedAuthHeader != "" {
		t.Errorf("expected no auth header, got %q", receivedAuthHeader)
	}
}

func TestPeer_ServeHTTP_HostHeaderSet(t *testing.T) {
	var receivedHost string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if !strings.HasPrefix(receivedHost, "127.0.0.1:") {
		t.Errorf("expected Host to start with '127.0.0.1:', got %q", receivedHost)
	}
}

func TestPeer_ServeHTTP_SSEHeaderModification(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("expected X-Accel-Buffering=no, got %q", w.Header().Get("X-Accel-Buffering"))
	}
}

func TestPeer_ServeHTTP_ShutdownRejectsNewRequests(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	err = pr.Shutdown(0)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "shutting down") {
		t.Errorf("expected 'shutting down' in body, got %q", w.Body.String())
	}
}

func TestPeer_ServeHTTP_WaitsForInflightDuringShutdown(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-released
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := httptest.NewRecorder()
		pr.ServeHTTP(w, req)
	}()

	<-started

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- pr.Shutdown(500 * time.Millisecond)
	}()

	// Shutdown should be waiting on inflight. If it finished already something is wrong.
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-shutdownDone:
		t.Errorf("shutdown completed before inflight finished: %v", err)
	default:
	}

	close(released)
	wg.Wait()

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Errorf("shutdown errored after inflight completed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("shutdown did not complete after inflight finished")
	}
}

func TestPeer_ServeHTTP_ShutdownTimeoutCancelsInflight(t *testing.T) {
	started := make(chan struct{})
	released := make(chan struct{})
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-released
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"test-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "test-model", ModelID: "test-model"}))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := httptest.NewRecorder()
		pr.ServeHTTP(w, req)
	}()

	<-started

	err = pr.Shutdown(100 * time.Millisecond)
	if err == nil {
		t.Error("expected timeout error from shutdown")
	}

	close(released)
	wg.Wait()
}

func TestPeer_ShutdownTimeoutBoundsInflightWait(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	pr.inflight.Add(1)
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- pr.Shutdown(50 * time.Millisecond)
	}()

	select {
	case err := <-shutdownDone:
		if err == nil || !strings.Contains(err.Error(), "peer shutdown timed out") {
			t.Fatalf("Shutdown error = %v, want timeout", err)
		}
	case <-time.After(time.Second):
		pr.inflight.Done()
		t.Fatal("Shutdown remained blocked on an inflight request after its deadline")
	}
	pr.inflight.Done()
}

func TestPeer_ShutdownMultiple(t *testing.T) {
	pr, err := NewPeer(config.Config{}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	err = pr.Shutdown(0)
	if err != nil {
		t.Fatal(err)
	}

	err = pr.Shutdown(0)
	if err == nil {
		t.Error("expected error on second shutdown")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("expected 'already in progress', got %q", err.Error())
	}
}

func TestPeer_ServeHTTP_ModelExtractedFromBody(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"extracted-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"model":"extracted-model","prompt":"hello"}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestPeer_ServeHTTP_ContextOverridesBodyModel(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	peers := config.PeerDictionaryConfig{
		"peer1": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"context-model"},
		},
		"peer2": config.PeerConfig{
			Proxy:    testServer.URL,
			ProxyURL: proxyURL,
			Models:   []string{"body-model"},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"model":"body-model","prompt":"hello"}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "context-model", ModelID: "context-model"}))
	w := httptest.NewRecorder()

	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNewPeer_CustomTimeouts(t *testing.T) {
	proxyURL, _ := url.Parse("http://localhost:8080")
	peers := config.PeerDictionaryConfig{
		"test-peer": config.PeerConfig{
			Proxy:    "http://localhost:8080",
			ProxyURL: proxyURL,
			Models:   []string{"model1"},
			Timeouts: config.TimeoutsConfig{
				Connect:        45,
				ResponseHeader: 300,
				TLSHandshake:   15,
				ExpectContinue: 2,
				IdleConn:       120,
			},
		},
	}

	pr, err := NewPeer(config.Config{Peers: peers}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	member, ok := pr.currentRoutes()["model1"]
	if !ok {
		t.Fatal("expected model1 to be mapped")
	}

	transport, ok := member.member.reverseProxy.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected Transport to be *http.Transport")
	}

	if transport.ResponseHeaderTimeout != 300*time.Second {
		t.Errorf("expected ResponseHeaderTimeout=%v, got %v", 300*time.Second, transport.ResponseHeaderTimeout)
	}
	if transport.TLSHandshakeTimeout != 15*time.Second {
		t.Errorf("expected TLSHandshakeTimeout=%v, got %v", 15*time.Second, transport.TLSHandshakeTimeout)
	}
	if transport.ExpectContinueTimeout != 2*time.Second {
		t.Errorf("expected ExpectContinueTimeout=%v, got %v", 2*time.Second, transport.ExpectContinueTimeout)
	}
	if transport.IdleConnTimeout != 120*time.Second {
		t.Errorf("expected IdleConnTimeout=%v, got %v", 120*time.Second, transport.IdleConnTimeout)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Error("expected ForceAttemptHTTP2 to be true")
	}
}

func TestNewPeer_TailcatTransportDisablesEnvironmentProxyAndCleansUp(t *testing.T) {
	private := tailcat.NewPrivateKey()
	private.Public.RegionID = 1
	blob := private.Public.ConnBlob()
	cfg, err := config.LoadConfigFromReader(strings.NewReader(`
models: {}
peers:
  cat:
    proxy: tailcat://` + string(blob) + `
    models: [remote]
`))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := NewPeer(cfg, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	member := pr.currentRoutes()["cat/remote"].member
	if member.tailcat == nil {
		t.Fatal("Tailcat client was not attached")
	}
	if member.transport.Proxy != nil {
		t.Fatal("Tailcat transport must not use environment HTTP proxies")
	}
	if member.reverseProxy.Transport != member.transport {
		t.Fatal("reverse proxy does not use the Tailcat transport")
	}
	if err := pr.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNewPeer_DiscoveryPopulatesRoutesEndToEnd(t *testing.T) {
	var upstreamModel string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"object":"list","data":[{"id":"discovered-model"}]}`))
			return
		}
		data, _ := swaputil.ExtractModel(r)
		upstreamModel = data
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	discovery := config.DefaultPeerDiscoveryConfig()
	discovery.RefreshInterval = 0 // fetch once, no ticker

	pr, err := NewPeer(config.Config{
		PeerModels: config.NewPeerRegistry(),
		Peers: config.PeerDictionaryConfig{
			"peer1": {
				Proxy:     testServer.URL,
				ProxyURL:  proxyURL,
				Discovery: &discovery,
			},
		},
	}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Shutdown(0)

	require.Eventually(t, func() bool {
		return pr.Handles("peer1/discovered-model")
	}, time.Second, 5*time.Millisecond, "discovered model should become routable")

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"peer1/discovered-model","prompt":"hello"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamModel != "discovered-model" {
		t.Fatalf("upstream model = %q, want discovered-model (unqualified)", upstreamModel)
	}
}

func TestNewPeer_DiscoveryDoesNotCreateBareName(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"only-one-provider"}]}`))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	discovery := config.DefaultPeerDiscoveryConfig()
	discovery.RefreshInterval = 0

	pr, err := NewPeer(config.Config{
		PeerModels: config.NewPeerRegistry(),
		Peers: config.PeerDictionaryConfig{
			"peer1": {
				Proxy:     testServer.URL,
				ProxyURL:  proxyURL,
				Discovery: &discovery,
			},
		},
	}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Shutdown(0)

	require.Eventually(t, func() bool {
		return pr.Handles("peer1/only-one-provider")
	}, time.Second, 5*time.Millisecond, "discovered model should become routable via FQN")

	// Per design decision D1, a discovered model - even one served by
	// exactly one peer - must never be reachable by its bare name, unlike
	// statically configured peer.models.
	if pr.Handles("only-one-provider") {
		t.Fatal("discovered model must not be reachable by a bare name")
	}
}

func TestNewPeer_DiscoverySkipsReservedName(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"local-model"}]}`))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	discovery := config.DefaultPeerDiscoveryConfig()
	discovery.RefreshInterval = 0

	cfg := config.Config{
		PeerModels: config.NewPeerRegistry(),
		Models:     map[string]config.ModelConfig{"local-model": {}},
		Peers: config.PeerDictionaryConfig{
			"local-model": { // peer ID intentionally matches a local model ID
				Proxy:     testServer.URL,
				ProxyURL:  proxyURL,
				Discovery: &discovery,
			},
		},
	}

	pr, err := NewPeer(cfg, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Shutdown(0)

	// "local-model/local-model" doesn't conflict with anything, so give the
	// poller a moment and confirm nothing panics and nothing unexpected
	// gets registered under the reserved bare name "local-model".
	time.Sleep(50 * time.Millisecond)
	if pr.Handles("local-model") {
		t.Fatal("discovered routes must never claim a name reserved by a local model")
	}
}

func TestPeer_ServeHTTP_DiscoveredModelUsesPeerApiKey(t *testing.T) {
	var gotAuth string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"object":"list","data":[{"id":"discovered-model"}]}`))
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	discovery := config.DefaultPeerDiscoveryConfig()
	discovery.RefreshInterval = 0

	pr, err := NewPeer(config.Config{
		PeerModels: config.NewPeerRegistry(),
		Peers: config.PeerDictionaryConfig{
			"peer1": {
				Proxy:     testServer.URL,
				ProxyURL:  proxyURL,
				ApiKey:    "sk-peer-key",
				Discovery: &discovery,
			},
		},
	}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Shutdown(0)

	require.Eventually(t, func() bool {
		return pr.Handles("peer1/discovered-model")
	}, time.Second, 5*time.Millisecond)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	*req = *req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{
		Model: "peer1/discovered-model", ModelID: "peer1/discovered-model",
	}))
	w := httptest.NewRecorder()
	pr.ServeHTTP(w, req)

	if gotAuth != "Bearer sk-peer-key" {
		t.Fatalf("expected peer's own apiKey to be injected for a discovered model, got %q", gotAuth)
	}
}

func TestNewPeer_DiscoveryFailureKeepsPreviousModels(t *testing.T) {
	var succeed atomic.Bool
	succeed.Store(true)

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !succeed.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"stable-model"}]}`))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	discovery := config.DefaultPeerDiscoveryConfig()
	discovery.RefreshInterval = 1 // seconds - fast enough to observe a second cycle in tests

	pr, err := NewPeer(config.Config{
		PeerModels: config.NewPeerRegistry(),
		Peers: config.PeerDictionaryConfig{
			"peer1": {
				Proxy:     testServer.URL,
				ProxyURL:  proxyURL,
				Discovery: &discovery,
			},
		},
	}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Shutdown(0)

	require.Eventually(t, func() bool {
		return pr.Handles("peer1/stable-model")
	}, time.Second, 5*time.Millisecond, "first fetch should succeed")

	succeed.Store(false)
	// Give at least one more refresh cycle a chance to run and fail.
	time.Sleep(1500 * time.Millisecond)

	if !pr.Handles("peer1/stable-model") {
		t.Fatal("a failed refresh must not remove previously discovered models")
	}
}

func TestNewPeer_DiscoveryEmitsPeerModelsChangedEvent(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer testServer.Close()

	var gotPeerID atomic.Value
	unsubscribe := event.On(func(e swaputil.PeerModelsChangedEvent) {
		gotPeerID.Store(e.PeerID)
	})
	defer unsubscribe()

	proxyURL, _ := url.Parse(testServer.URL)
	discovery := config.DefaultPeerDiscoveryConfig()
	discovery.RefreshInterval = 0

	pr, err := NewPeer(config.Config{
		PeerModels: config.NewPeerRegistry(),
		Peers: config.PeerDictionaryConfig{
			"peer1": {
				Proxy:     testServer.URL,
				ProxyURL:  proxyURL,
				Discovery: &discovery,
			},
		},
	}, testLogger)
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Shutdown(0)

	require.Eventually(t, func() bool {
		v, ok := gotPeerID.Load().(string)
		return ok && v == "peer1"
	}, time.Second, 5*time.Millisecond, "expected a PeerModelsChangedEvent for peer1")
}

func TestPeer_Shutdown_StopsDiscoveryPolling(t *testing.T) {
	var requestCount atomic.Int64
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Write([]byte(`{"object":"list","data":[{"id":"m"}]}`))
	}))
	defer testServer.Close()

	proxyURL, _ := url.Parse(testServer.URL)
	discovery := config.DefaultPeerDiscoveryConfig()
	discovery.RefreshInterval = 1 // seconds

	pr, err := NewPeer(config.Config{
		PeerModels: config.NewPeerRegistry(),
		Peers: config.PeerDictionaryConfig{
			"peer1": {
				Proxy:     testServer.URL,
				ProxyURL:  proxyURL,
				Discovery: &discovery,
			},
		},
	}, testLogger)
	if err != nil {
		t.Fatal(err)
	}

	require.Eventually(t, func() bool {
		return requestCount.Load() >= 1
	}, time.Second, 5*time.Millisecond)

	// Shutdown with a generous timeout and no in-flight requests: per D4.1,
	// discoveryCtx must be cancelled unconditionally and immediately, not
	// only on the timeout path (unlike shutdownCtx).
	if err := pr.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("unexpected shutdown error: %v", err)
	}

	countAtShutdown := requestCount.Load()
	time.Sleep(1500 * time.Millisecond) // longer than the 1s refresh interval
	if got := requestCount.Load(); got != countAtShutdown {
		t.Fatalf("discovery kept polling after Shutdown: %d requests before, %d after", countAtShutdown, got)
	}
}
