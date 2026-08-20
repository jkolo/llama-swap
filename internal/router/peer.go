package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/internal/tailcat"
)

// discoveryRequestTimeout bounds a single discovery HTTP request (including
// connection setup), independent of the peer's own transport timeouts -
// some peers configure "responseHeader: 0" (no timeout) on themselves,
// which would otherwise let a hanging /v1/models pin a discovery goroutine
// indefinitely.
const discoveryRequestTimeout = 30 * time.Second

type peerMember struct {
	peerID       string
	reverseProxy *httputil.ReverseProxy
	transport    *http.Transport
	tailcat      *tailcat.Client
	apiKey       string

	// baseURL is the peer's configured proxy URL, used as the base for
	// discovery requests (discovery.path is appended to it).
	baseURL string

	// httpClient reuses reverseProxy's own transport so discovery requests
	// share the same connection pool, TLS, and proxy settings as normal
	// proxied traffic to this peer.
	httpClient *http.Client
}

type peerRoute struct {
	member  *peerMember
	modelID string
}

// Peer proxies requests to models served by remote peers. Its route table
// is published behind an atomic.Pointer so ordinary request handling never
// blocks on - or is blocked by - a peer's background model discovery
// refresh; see currentRoutes/publishRoutes.
type Peer struct {
	cfg    config.Config
	logger *logmon.Monitor

	members       map[string]*peerMember // peerID -> member, built once, immutable after NewPeer
	staticRoutes  map[string]*peerRoute  // static peer.models routes (FQN + bare aliases), immutable after NewPeer
	reservedNames map[string]string      // computed once from cfg; see config.ReservedModelNames
	routes        atomic.Pointer[map[string]*peerRoute]

	shutdownCtx  context.Context
	shutdownFn   context.CancelFunc
	shuttingDown atomic.Bool
	inflight     sync.WaitGroup

	// discoveryCtx/discoveryCancel govern only the background discovery
	// pollers. They are deliberately separate from shutdownCtx: shutdownCtx
	// is cancelled conditionally (only on a Shutdown timeout, so in-flight
	// proxied requests can drain gracefully - see Shutdown), whereas
	// discovery pollers must stop unconditionally and immediately whenever
	// Shutdown is called, or every config reload leaks one goroutine per
	// discovery-enabled peer.
	discoveryCtx    context.Context
	discoveryCancel context.CancelFunc
	discoveryWG     sync.WaitGroup
}

func NewPeer(cfg config.Config, logger *logmon.Monitor) (*Peer, error) {
	if err := config.ValidatePeerNamespace(cfg); err != nil {
		return nil, err
	}

	// LoadConfigFromReader always allocates PeerModels, but cfg may also be
	// built by hand (tests, or any future caller). Rather than let
	// discovery silently no-op against a nil registry (PeerRegistry's
	// methods are nil-safe by design, precisely so bare config.Config{}
	// literals elsewhere in the codebase don't panic), default to a fresh
	// one here so a discovery-enabled peer always has somewhere to publish
	// its results.
	if cfg.PeerModels == nil {
		cfg.PeerModels = config.NewPeerRegistry()
	}

	peers := cfg.Peers
	members := make(map[string]*peerMember, len(peers))
	staticRoutes := make(map[string]*peerRoute)
	bareRoutes := make(map[string][]*peerRoute)

	peerIDs := make([]string, 0, len(peers))
	tailcatClients := make(map[string]*tailcat.Client)
	for peerID := range peers {
		peerIDs = append(peerIDs, peerID)
	}
	sort.Strings(peerIDs)

	for _, peerID := range peerIDs {
		peer := peers[peerID]
		pp := newPeerMember(peerID, peer, logger, tailcatClients)
		members[peerID] = pp

		seen := make(map[string]struct{})
		for _, modelID := range peer.Models {
			if _, duplicate := seen[modelID]; duplicate {
				continue
			}
			seen[modelID] = struct{}{}

			route := &peerRoute{member: pp, modelID: modelID}
			staticRoutes[config.PeerModelFQN(peerID, modelID)] = route
			bareRoutes[modelID] = append(bareRoutes[modelID], route)
		}
	}

	for modelID, candidates := range bareRoutes {
		if len(candidates) != 1 {
			continue
		}
		if _, reserved := staticRoutes[modelID]; reserved {
			continue
		}
		staticRoutes[modelID] = candidates[0]
	}

	shutdownCtx, shutdownFn := context.WithCancel(context.Background())
	discoveryCtx, discoveryCancel := context.WithCancel(context.Background())

	initialRoutes := make(map[string]*peerRoute, len(staticRoutes))
	for k, v := range staticRoutes {
		initialRoutes[k] = v
	}

	r := &Peer{
		cfg:             cfg,
		logger:          logger,
		members:         members,
		staticRoutes:    staticRoutes,
		reservedNames:   config.ReservedModelNames(cfg),
		shutdownCtx:     shutdownCtx,
		shutdownFn:      shutdownFn,
		discoveryCtx:    discoveryCtx,
		discoveryCancel: discoveryCancel,
	}
	r.routes.Store(&initialRoutes)

	for _, peerID := range peerIDs {
		peer := peers[peerID]
		if peer.Discovery == nil || !peer.Discovery.Enabled {
			continue
		}
		r.startDiscovery(peerID, *peer.Discovery, members[peerID])
	}

	return r, nil
}

// newPeerMember builds the reverse proxy and (for discovery) the plain
// http.Client used to reach one peer. Both share the same *http.Transport
// so proxied requests and discovery requests use the same connection pool,
// TLS, and dialer/timeout settings.
func newPeerMember(peerID string, peer config.PeerConfig, logger *logmon.Monitor, tailcatClients map[string]*tailcat.Client) *peerMember {
	var tailcatClient *tailcat.Client
	proxyFromEnvironment := http.ProxyFromEnvironment
	dialContext := (&net.Dialer{
		Timeout:   time.Duration(peer.Timeouts.Connect) * time.Second,
		KeepAlive: time.Duration(peer.Timeouts.KeepAlive) * time.Second,
	}).DialContext
	if _, blob, privateKey, found := peer.Tailcat(); found {
		clientKey := "ephemeral:" + peerID
		if privateKey != nil {
			clientKey = privateKey.Identity() + ":" + blob
		}
		tailcatClient = tailcatClients[clientKey]
		if tailcatClient == nil {
			tailcatClient = tailcat.NewClient(peerID, blob, privateKey, logger)
			tailcatClients[clientKey] = tailcatClient
		}
		proxyFromEnvironment = nil
		dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if peer.Timeouts.Connect > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(peer.Timeouts.Connect)*time.Second)
				defer cancel()
			}
			return tailcatClient.DialContext(ctx, network, address)
		}
	}

	peerTransport := &http.Transport{
		Proxy:                 proxyFromEnvironment,
		DialContext:           dialContext,
		TLSHandshakeTimeout:   time.Duration(peer.Timeouts.TLSHandshake) * time.Second,
		ResponseHeaderTimeout: time.Duration(peer.Timeouts.ResponseHeader) * time.Second,
		ExpectContinueTimeout: time.Duration(peer.Timeouts.ExpectContinue) * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       time.Duration(peer.Timeouts.IdleConn) * time.Second,
	}

	reverseProxy := &httputil.ReverseProxy{
		Transport: peerTransport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(peer.ProxyURL)
			r.Out.Host = r.Out.URL.Host
		},
	}

	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Header.Set("X-Accel-Buffering", "no")
		}
		return nil
	}

	reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// A cancelled request is not a peer failure, so keep it out of the
		// warning stream whether or not the sentinel applies below.
		if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
			logger.Debugf("peer %s: request cancelled: %v", peerID, err)
		} else {
			logger.Warnf("peer %s: proxy error: %v", peerID, err)
		}

		// Only a client that actually hung up gets the recorded-only
		// sentinel (#1029). A request cancelled server-side still has a
		// client waiting for an answer.
		if swaputil.MarkClientClosed(w, r) || swaputil.ResponseStarted(w) {
			return
		}

		errMsg := fmt.Sprintf("peer proxy error: %v", err)
		if runtime.GOOS == "darwin" && strings.Contains(err.Error(), "connect: no route to host") {
			errMsg += " (hint: on macOS, check System Settings > Privacy & Security > Local Network permissions)"
		}
		swaputil.SendResponse(w, r, http.StatusBadGateway, errMsg)
	}

	return &peerMember{
		peerID:       peerID,
		reverseProxy: reverseProxy,
		transport:    peerTransport,
		tailcat:      tailcatClient,
		apiKey:       peer.ApiKey,
		baseURL:      peer.Proxy,
		httpClient:   &http.Client{Transport: peerTransport},
	}
}

// startDiscovery launches the background poller for one peer: an immediate
// first fetch, then (when discovery.RefreshInterval > 0) a repeating fetch
// on that interval until discoveryCtx is cancelled. RefreshInterval == 0
// means "only at startup and config reload" - a single fetch, no ticker.
func (r *Peer) startDiscovery(peerID string, discovery config.PeerDiscoveryConfig, member *peerMember) {
	r.discoveryWG.Add(1)
	go func() {
		defer r.discoveryWG.Done()

		r.refreshDiscovery(peerID, discovery, member)
		if discovery.RefreshInterval <= 0 {
			return
		}

		ticker := time.NewTicker(time.Duration(discovery.RefreshInterval) * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.discoveryCtx.Done():
				return
			case <-ticker.C:
				r.refreshDiscovery(peerID, discovery, member)
			}
		}
	}()
}

// refreshDiscovery performs one discovery fetch for a peer. On success, it
// filters out any discovered model ID that collides with a reserved name,
// publishes the result to the shared registry, and republishes the full
// merged route table. On failure, it logs a warning and leaves the
// registry (and therefore the route table) untouched, per the "log and
// keep the previous known-good set" policy - an unreachable peer must not
// make its previously discovered models stop working.
func (r *Peer) refreshDiscovery(peerID string, discovery config.PeerDiscoveryConfig, member *peerMember) {
	ctx, cancel := context.WithTimeout(r.discoveryCtx, discoveryRequestTimeout)
	defer cancel()

	discovered, err := FetchDiscoveredModels(ctx, member.httpClient, peerID, member.baseURL, discovery, member.apiKey)
	if err != nil {
		r.logger.Warnf("peer %s: model discovery failed: %v", peerID, err)
		return
	}

	kept := make(map[string]config.DiscoveredModel, len(discovered))
	for modelID, dm := range discovered {
		fqn := config.PeerModelFQN(peerID, modelID)
		if reason, blocked := r.reservedNames[fqn]; blocked {
			r.logger.Warnf("peer %s: discovered model %q skipped: %s conflicts with fully qualified peer model name %q", peerID, modelID, reason, fqn)
			continue
		}
		kept[modelID] = dm
	}

	r.cfg.PeerModels.SetPeerModels(peerID, kept)
	r.logger.Debugf("peer %s: discovered %d model(s)", peerID, len(kept))

	r.publishRoutes()
	event.Emit(swaputil.PeerModelsChangedEvent{PeerID: peerID})
}

// publishRoutes rebuilds the full route table from the immutable static
// routes plus every currently discovered model across all peers, and
// atomically swaps it in. This always recomputes from the complete
// registry snapshot (not just the peer that just refreshed), so one peer's
// refresh can never leave another peer's previously published routes
// stale relative to the registry.
//
// Static routes always take precedence over a same-named discovered entry
// (mirrors ResolvePeerModel's precedence) and discovered entries are never
// added as bare names (see design decision D1) - so this merge can never
// introduce ambiguity, unlike the bare-name collapse in NewPeer.
func (r *Peer) publishRoutes() {
	discovered := r.cfg.PeerModels.Models()

	next := make(map[string]*peerRoute, len(r.staticRoutes)+len(discovered))
	for k, v := range r.staticRoutes {
		next[k] = v
	}
	for _, dm := range discovered {
		fqn := config.PeerModelFQN(dm.PeerID, dm.ModelID)
		if _, exists := next[fqn]; exists {
			continue
		}
		member, ok := r.members[dm.PeerID]
		if !ok {
			continue
		}
		next[fqn] = &peerRoute{member: member, modelID: dm.ModelID}
	}

	r.routes.Store(&next)
}

// currentRoutes returns the currently published route table.
func (r *Peer) currentRoutes() map[string]*peerRoute {
	routes := r.routes.Load()
	if routes == nil {
		return nil
	}
	return *routes
}

func (r *Peer) Handles(model string) bool {
	_, ok := r.currentRoutes()[model]
	return ok
}

func (r *Peer) Shutdown(timeout time.Duration) error {
	if !r.shuttingDown.CompareAndSwap(false, true) {
		return fmt.Errorf("shutdown already in progress")
	}

	// Discovery pollers stop unconditionally and immediately - unlike
	// shutdownCtx below, there is no reason to let a background /v1/models
	// poll linger while requests drain.
	r.discoveryCancel()
	r.discoveryWG.Wait()

	if timeout == 0 {
		r.shutdownFn()
		r.inflight.Wait()
		return r.closeTransports(time.Second)
	}

	deadline := time.Now().Add(timeout)
	deadlineCtx, cancelDeadline := context.WithDeadline(context.Background(), deadline)
	defer cancelDeadline()

	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		r.shutdownFn()
		return r.closeTransports(max(time.Until(deadline), 0))
	case <-deadlineCtx.Done():
		r.shutdownFn()
		select {
		case <-done:
		case <-deadlineCtx.Done():
		}
		return errors.Join(fmt.Errorf("peer shutdown timed out after %v", timeout), r.closeTransports(max(time.Until(deadline), 0)))
	}
}

// closeTransports releases each peer's idle connections and, for Tailcat
// peers, closes the shared Tailcat client. members is keyed by peer ID
// rather than being a slice, so the per-member error slots are handed out
// by iteration order - which one a given peer gets does not matter, they
// are only joined at the end.
func (r *Peer) closeTransports(timeout time.Duration) error {
	type closeResult struct {
		index int
		err   error
	}

	deadline := time.Now().Add(timeout)
	closedTailcat := make(map[*tailcat.Client]struct{})
	errs := make([]error, len(r.members))
	done := make(chan closeResult, len(r.members))
	index := 0
	for _, member := range r.members {
		slot := index
		index++
		closeTailcat := false
		if member.tailcat != nil {
			if _, closed := closedTailcat[member.tailcat]; !closed {
				closedTailcat[member.tailcat] = struct{}{}
				closeTailcat = true
			}
		}

		go func() {
			var err error
			member.transport.CloseIdleConnections()
			if closeTailcat {
				if closeErr := member.tailcat.CloseWithTimeout(max(time.Until(deadline), 0)); closeErr != nil {
					err = fmt.Errorf("peer %s: %w", member.peerID, closeErr)
				}
			}
			done <- closeResult{index: slot, err: err}
		}()
	}

	timer := time.NewTimer(max(time.Until(deadline), 0))
	defer timer.Stop()
	for range r.members {
		select {
		case result := <-done:
			errs[result.index] = result.err
		case <-timer.C:
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

func (r *Peer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.shuttingDown.Load() {
		swaputil.SendError(w, req, fmt.Errorf("peer proxy is shutting down"))
		return
	}
	r.inflight.Add(1)
	defer r.inflight.Done()

	data, err := swaputil.FetchContext(req, r.cfg)
	if err != nil {
		swaputil.SendError(w, req, err)
		return
	}

	route, found := r.currentRoutes()[data.ModelID]
	if !found {
		r.logger.Warnf("peer model not found: %s", data.ModelID)
		swaputil.SendError(w, req, ErrNoPeerModelFound)
		return
	}
	pp := route.member

	r.logger.Debugf("peer: routing model %s to peer %s as %s", data.ModelID, pp.peerID, route.modelID)

	if data.Model != route.modelID {
		req, err = swaputil.ReplaceRequestModel(req, data.Model, route.modelID)
		if err != nil {
			swaputil.SendResponse(w, req, http.StatusBadRequest, err.Error())
			return
		}
	}

	if pp.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+pp.apiKey)
		req.Header.Set("x-api-key", pp.apiKey)
	}

	// Cancel the proxy request when the client disconnects or shutdown times out.
	// Deriving from the request covers the client half directly and keeps the
	// request's context values — notably the client context that tells a real
	// disconnect apart from a server-side cancel. AfterFunc links the unrelated
	// shutdown context in without a goroutine leak.
	ctx, cancel := context.WithCancel(req.Context())
	stopShutdown := context.AfterFunc(r.shutdownCtx, cancel)
	req = req.WithContext(ctx)

	pp.reverseProxy.ServeHTTP(w, req)

	stopShutdown()
	cancel()
}
