package service

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/panel/port"
)

// ChainHealth is the verdict on one relay chain's path, as seen from the node
// that has to carry it.
type ChainHealth struct {
	OK        bool      `json:"ok"`
	Target    string    `json:"target,omitempty"` // host:port the relay forwards to
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// ChainProber asks one node to dial a target.
type ChainProber interface {
	Probe(ctx context.Context, nodeID uuid.UUID, target string, timeout time.Duration) (ok bool, latency time.Duration, errMsg string, err error)
}

// ChainHealthService probes every managed chain on a timer and keeps the last
// result in memory, so listing chains never blocks on the network.
//
// The probe runs on the entry node, not on the panel: the question is whether
// that machine can reach the next hop, which the panel's own vantage point
// cannot answer.
type ChainHealthService struct {
	chains   port.RelayChainRepository
	nodes    nodeLookup
	inbounds inboundLister
	prober   ChainProber
	log      *slog.Logger

	timeout  time.Duration
	interval time.Duration
	now      func() time.Time

	mu    sync.RWMutex
	cache map[uuid.UUID]ChainHealth
}

// NewChainHealthService wires the checker. A nil prober disables it.
func NewChainHealthService(chains port.RelayChainRepository, nodes nodeLookup, inbounds inboundLister, prober ChainProber, log *slog.Logger) *ChainHealthService {
	return &ChainHealthService{
		chains:   chains,
		nodes:    nodes,
		inbounds: inbounds,
		prober:   prober,
		log:      log,
		timeout:  5 * time.Second,
		interval: time.Minute,
		now:      time.Now,
		cache:    map[uuid.UUID]ChainHealth{},
	}
}

// SetInterval overrides the background cadence (tests use a short one).
func (s *ChainHealthService) SetInterval(d time.Duration) {
	if d > 0 {
		s.interval = d
	}
}

// Snapshot returns the last known health per chain id.
func (s *ChainHealthService) Snapshot() map[uuid.UUID]ChainHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[uuid.UUID]ChainHealth, len(s.cache))
	for k, v := range s.cache {
		out[k] = v
	}
	return out
}

// Health returns the cached verdict for one chain.
func (s *ChainHealthService) Health(chainID uuid.UUID) (ChainHealth, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.cache[chainID]
	return h, ok
}

// Check probes one chain now and records the result. It returns false for chains
// that have no path to check (disabled, or an external hop the panel does not
// run), and forgets any stale verdict for them.
func (s *ChainHealthService) Check(ctx context.Context, chain *domain.RelayChain) (ChainHealth, bool) {
	if s == nil || s.prober == nil || chain == nil {
		return ChainHealth{}, false
	}
	hop, managed := managedHop(chain)
	if !managed || !chain.Enabled {
		s.forget(chain.ID)
		return ChainHealth{}, false
	}
	h := ChainHealth{CheckedAt: s.now()}
	host, port, err := resolveChainTarget(ctx, s.nodes, s.inbounds, chain)
	if err != nil {
		h.Error = err.Error()
	} else {
		h.Target = net.JoinHostPort(host, strconv.Itoa(port))
		ok, latency, msg, perr := s.prober.Probe(ctx, *hop.NodeID, h.Target, s.timeout)
		h.OK = ok
		h.LatencyMS = latency.Milliseconds()
		switch {
		case perr != nil:
			// A transport failure is not a verdict; never leave it looking green.
			h.OK = false
			h.Error = "entry node unreachable: " + perr.Error()
		case !ok:
			h.Error = msg
			if h.Error == "" {
				h.Error = "connection failed"
			}
		}
	}
	s.mu.Lock()
	s.cache[chain.ID] = h
	s.mu.Unlock()
	return h, true
}

// CheckID probes one stored chain.
func (s *ChainHealthService) CheckID(ctx context.Context, id uuid.UUID) (ChainHealth, bool, error) {
	chain, err := s.chains.GetByID(ctx, id)
	if err != nil {
		return ChainHealth{}, false, err
	}
	h, ok := s.Check(ctx, chain)
	return h, ok, nil
}

// Run probes every chain once, then on the configured interval until ctx ends.
func (s *ChainHealthService) Run(ctx context.Context) {
	if s == nil || s.prober == nil {
		return
	}
	s.checkAll(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkAll(ctx)
		}
	}
}

func (s *ChainHealthService) checkAll(ctx context.Context) {
	chains, err := s.chains.List(ctx)
	if err != nil {
		s.log.Warn("relay health: list chains failed", "err", err)
		return
	}
	for _, c := range chains {
		cctx, cancel := context.WithTimeout(ctx, s.timeout+5*time.Second)
		s.Check(cctx, c)
		cancel()
	}
}

func (s *ChainHealthService) forget(id uuid.UUID) {
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()
}
