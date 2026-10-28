package grpc

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"

	"github.com/vortexui/vortexui/internal/core"
	"github.com/vortexui/vortexui/internal/decoy"
	"github.com/vortexui/vortexui/internal/domain"
	"github.com/vortexui/vortexui/internal/pki"
	genv1 "github.com/vortexui/vortexui/internal/transport/genv1"
)

// NodeServer adapts a core.CoreDriver to the NodeService gRPC contract. It is
// the process boundary on each proxy server: the panel's calls land here and are
// translated into driver operations.
type NodeServer struct {
	genv1.UnimplementedNodeServiceServer
	driver   core.CoreDriver
	agentVer string
	decoy    *decoy.Server
	certs    *CertReloader

	// What the agent learned about its core binary at startup: the panel needs
	// the build tags to know whether per-user traffic can be counted at all.
	coreVersion string
	coreTags    string
	// installCore is supplied by the agent binary; it swaps the core binary.
	// installCore returns handledRestart=true when the agent restarted itself
	// (the config shape depends on the new binary), so the caller must not also
	// reload the running driver.
	installCore func(ctx context.Context, core, version, url, sha string, allowUnverified bool) (bool, error)
	// upgradeAgent replaces the agent's own binary; it schedules its own restart.
	upgradeAgent func(ctx context.Context, url, sha string) error

	mu  sync.Mutex
	srv *grpc.Server
}

// NewNodeServer wraps a driver. The driver owns the actual proxy engine.
func NewNodeServer(driver core.CoreDriver, agentVer string, decoySrv *decoy.Server) *NodeServer {
	return &NodeServer{driver: driver, agentVer: agentVer, decoy: decoySrv}
}

// Serve registers the service and blocks serving on lis until GracefulStop is
// called or the listener fails. Pass server options such as mTLS credentials.
func (s *NodeServer) Serve(lis net.Listener, opts ...grpc.ServerOption) error {
	srv := grpc.NewServer(opts...)
	genv1.RegisterNodeServiceServer(srv, s)
	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()
	return srv.Serve(lis)
}

// GracefulStop stops the server, letting in-flight RPCs finish. Safe to call
// before Serve (no-op) and from another goroutine.
func (s *NodeServer) GracefulStop() {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		srv.GracefulStop()
	}
}

// SetCoreInstaller lets the panel replace the core binary on this node.
func (s *NodeServer) SetCoreInstaller(fn func(ctx context.Context, core, version, url, sha string, allowUnverified bool) (bool, error)) {
	s.installCore = fn
}

// SetAgentUpgrader lets the panel replace this agent's own binary.
func (s *NodeServer) SetAgentUpgrader(fn func(ctx context.Context, url, sha string) error) {
	s.upgradeAgent = fn
}

// SetCoreReport records the primary core's version and build tags.
func (s *NodeServer) SetCoreReport(version, tags string) {
	s.coreVersion = version
	s.coreTags = tags
}

// SetCertReloader lets the agent accept a rotated certificate at runtime.
func (s *NodeServer) SetCertReloader(r *CertReloader) { s.certs = r }

// InstallCertificate validates a new identity, persists it and hot-swaps it.
func (s *NodeServer) InstallCertificate(_ context.Context, req *genv1.InstallCertificateRequest) (*genv1.Ack, error) {
	if s.certs == nil {
		return ack(false, "this agent build cannot reload its certificate"), nil
	}
	if len(req.GetCertPem()) == 0 || len(req.GetKeyPem()) == 0 {
		return ack(false, "cert_pem and key_pem are required"), nil
	}
	if want := strings.TrimSpace(req.GetFingerprint()); want != "" {
		got, err := pki.FingerprintPEM(req.GetCertPem())
		if err != nil {
			return ack(false, "invalid certificate: "+err.Error()), nil
		}
		if !strings.EqualFold(got, want) {
			return ack(false, "certificate fingerprint mismatch"), nil
		}
	}
	if err := s.certs.Reload(req.GetCertPem(), req.GetKeyPem()); err != nil {
		return ack(false, "install certificate: "+err.Error()), nil
	}
	return ack(true, "certificate installed"), nil
}

// InstallCore swaps the core binary and restarts the engine. The agent rolls
// back on its own if the artifact cannot be verified or started.
func (s *NodeServer) InstallCore(ctx context.Context, req *genv1.InstallCoreRequest) (*genv1.Ack, error) {
	if s.installCore == nil {
		return ack(false, "this agent build cannot install cores"), nil
	}
	handledRestart, err := s.installCore(ctx, req.GetCore(), req.GetVersion(), req.GetUrl(), req.GetSha256(), req.GetAllowUnverified())
	if err != nil {
		return ack(false, err.Error()), nil
	}
	// The new binary is on disk; restart the engine so it is the one running —
	// unless the agent is restarting itself to rebuild a config that must now
	// omit (or re-include) the stats block.
	if !handledRestart {
		if err := s.driver.Reload(ctx, nil); err != nil {
			return ack(false, "installed, but restarting the core failed: "+err.Error()), nil
		}
	}
	s.coreVersion = req.GetVersion()
	return ack(true, "core "+req.GetCore()+" "+req.GetVersion()+" installed and restarted"), nil
}

// UpgradeAgent swaps the agent binary and restarts the process.
func (s *NodeServer) UpgradeAgent(ctx context.Context, req *genv1.UpgradeAgentRequest) (*genv1.Ack, error) {
	if s.upgradeAgent == nil {
		return ack(false, "this agent build cannot upgrade itself"), nil
	}
	if err := s.upgradeAgent(ctx, req.GetUrl(), req.GetSha256()); err != nil {
		return ack(false, err.Error()), nil
	}
	return ack(true, "agent upgraded; restarting"), nil
}

// Probe dials a TCP target from this node and reports whether it connected.
// The timeout is capped so a black-holed target cannot tie up the agent.
func (s *NodeServer) Probe(ctx context.Context, req *genv1.ProbeRequest) (*genv1.ProbeResponse, error) {
	target := strings.TrimSpace(req.GetTarget())
	if target == "" {
		return &genv1.ProbeResponse{Error: "target is required"}, nil
	}
	timeout := time.Duration(req.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", target)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return &genv1.ProbeResponse{LatencyMs: latency, Error: err.Error()}, nil
	}
	_ = conn.Close()
	return &genv1.ProbeResponse{Ok: true, LatencyMs: latency}, nil
}

func ack(ok bool, msg string) *genv1.Ack { return &genv1.Ack{Ok: ok, Message: msg} }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Sync rebuilds the engine-neutral config from the request and (re)starts the
// core. Start is idempotent, so this doubles as a hot reload.
func (s *NodeServer) Sync(ctx context.Context, req *genv1.SyncRequest) (*genv1.Ack, error) {
	reqCore := coreTypeFromProto(req.GetCore())
	drvCore := s.driver.Type()
	if reqCore != "" {
		switch {
		case reqCore == drvCore:
		case drvCore == domain.CoreMulti && reqCore == domain.CoreMulti:
		case drvCore == domain.CoreMulti && reqCore == "":
		default:
			if drvCore == domain.CoreMulti {
				return ack(false, fmt.Sprintf(
					"core mismatch: panel expects %s but agent runs multi-core — enable both engines on the node (VORTEX_ENABLED_CORES=xray,singbox)",
					reqCore,
				)), nil
			}
			return ack(false, fmt.Sprintf(
				"core mismatch: panel expects %s but agent runs %s — set VORTEX_CORE=%s on the node and restart vortexui-node",
				reqCore, drvCore, reqCore,
			)), nil
		}
	}
	if reqCore == domain.CoreMulti && drvCore != domain.CoreMulti {
		return ack(false, fmt.Sprintf(
			"core mismatch: panel expects multi-core but agent runs %s — set VORTEX_ENABLED_CORES=xray,singbox on the node",
			drvCore,
		)), nil
	}
	cfg := &core.GeneratedConfig{
		LogLevel:       req.GetLogLevel(),
		UsersByInbound: make(map[string][]*domain.User, len(req.GetUsersByInbound())),
	}
	for _, in := range req.GetInbounds() {
		cfg.Inbounds = append(cfg.Inbounds, inboundFromSpec(in))
	}
	for tag, list := range req.GetUsersByInbound() {
		users := make([]*domain.User, 0, len(list.GetUsers()))
		for _, u := range list.GetUsers() {
			users = append(users, userFromSpec(u))
		}
		cfg.UsersByInbound[tag] = users
	}
	for _, o := range req.GetOutbounds() {
		cfg.Outbounds = append(cfg.Outbounds, outboundFromSpec(o))
	}
	for _, r := range req.GetRouting() {
		cfg.Routing = append(cfg.Routing, routingFromSpec(r))
	}
	for _, b := range req.GetBalancers() {
		cfg.Balancers = append(cfg.Balancers, balancerFromSpec(b))
	}
	if err := s.driver.Start(ctx, cfg); err != nil {
		return ack(false, fmt.Sprintf("sync: %v", err)), nil
	}
	if s.decoy != nil {
		_ = s.decoy.ReloadFromInbounds(cfg.Inbounds)
	}
	return ack(true, ""), nil
}

func (s *NodeServer) AddUser(ctx context.Context, req *genv1.AddUserRequest) (*genv1.Ack, error) {
	if err := s.driver.AddUser(ctx, req.GetInboundTag(), userFromSpec(req.GetUser())); err != nil {
		return ack(false, err.Error()), nil
	}
	return ack(true, ""), nil
}

func (s *NodeServer) RemoveUser(ctx context.Context, req *genv1.RemoveUserRequest) (*genv1.Ack, error) {
	if err := s.driver.RemoveUser(ctx, req.GetInboundTag(), req.GetUserId()); err != nil {
		return ack(false, err.Error()), nil
	}
	return ack(true, ""), nil
}

// StreamTraffic bridges the driver's delta channel onto the gRPC stream. It ends
// when the driver channel closes or the panel disconnects (stream ctx done).
func (s *NodeServer) StreamTraffic(req *genv1.StreamTrafficRequest, stream grpc.ServerStreamingServer[genv1.TrafficDelta]) error {
	ch, err := s.driver.StreamTraffic(stream.Context())
	if err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case d, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(trafficToProto(d)); err != nil {
				return err
			}
		}
	}
}

func (s *NodeServer) Health(ctx context.Context, _ *genv1.HealthRequest) (*genv1.HealthResponse, error) {
	h, err := s.driver.Health(ctx)
	if err != nil {
		return nil, err
	}
	ver, _ := s.driver.Version(ctx)
	return &genv1.HealthResponse{
		CpuPercent:   h.CPUPercent,
		MemPercent:   h.MemPercent,
		DiskPercent:  h.DiskPercent,
		CoreRunning:  h.CoreRunning,
		Connections:  uint32(h.Connections),
		CoreVersion:  firstNonEmpty(ver, s.coreVersion),
		CoreTags:     s.coreTags,
		Arch:         runtime.GOARCH,
		AgentVersion: s.agentVer,
	}, nil
}

// RestartCore restarts the local proxy engine (hot-reload the config).
func (s *NodeServer) RestartCore(ctx context.Context, _ *genv1.RestartCoreRequest) (*genv1.Ack, error) {
	if err := s.driver.Reload(ctx, nil); err != nil {
		return ack(false, err.Error()), nil
	}
	return ack(true, "core restarted"), nil
}

// StopCore gracefully stops the local proxy engine.
func (s *NodeServer) StopCore(ctx context.Context, _ *genv1.StopCoreRequest) (*genv1.Ack, error) {
	if err := s.driver.Stop(ctx); err != nil {
		return ack(false, err.Error()), nil
	}
	return ack(true, "core stopped"), nil
}

// OnlineStats reports live per-user connection counts from the driver.
func (s *NodeServer) OnlineStats(ctx context.Context, _ *genv1.OnlineStatsRequest) (*genv1.OnlineStatsResponse, error) {	stats, err := s.driver.OnlineStats(ctx)
	if err != nil {
		return nil, err
	}
	online := make(map[string]uint32, len(stats))
	for email, n := range stats {
		online[email] = uint32(n)
	}
	return &genv1.OnlineStatsResponse{Online: online}, nil
}

// OnlineIPs reports the distinct source IPs currently online for one user.
func (s *NodeServer) OnlineIPs(ctx context.Context, req *genv1.OnlineIPsRequest) (*genv1.OnlineIPsResponse, error) {
	ips, err := s.driver.OnlineIPList(ctx, req.GetUserId())
	if err != nil {
		return nil, err
	}
	return &genv1.OnlineIPsResponse{Ips: ips}, nil
}

// UpdateGeo downloads geo routing databases on this node and reloads the core.
func (s *NodeServer) UpdateGeo(ctx context.Context, req *genv1.UpdateGeoRequest) (*genv1.UpdateGeoResponse, error) {
	geoip, geosite, err := s.driver.UpdateGeoAssets(ctx, req.GetGeoipUrl(), req.GetGeositeUrl())
	if err != nil {
		return nil, err
	}
	return &genv1.UpdateGeoResponse{GeoipBytes: geoip, GeositeBytes: geosite}, nil
}

// NodeLogs returns the agent's most recent captured core log lines.
func (s *NodeServer) NodeLogs(ctx context.Context, req *genv1.NodeLogsRequest) (*genv1.NodeLogsResponse, error) {
	lines, err := s.driver.Logs(ctx, int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	return &genv1.NodeLogsResponse{Lines: lines}, nil
}
