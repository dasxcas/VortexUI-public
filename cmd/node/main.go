// Command node is the VortexUI agent that runs on every proxy server. It serves
// the NodeService gRPC API over mTLS; the panel dials in to push config/users
// and to stream traffic. The agent drives xray-core or sing-box via VORTEX_CORE.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/vortexui/vortexui/internal/agent"
	"github.com/vortexui/vortexui/internal/config"
	"github.com/vortexui/vortexui/internal/decoy"
	vgrpc "github.com/vortexui/vortexui/internal/transport/grpc"
)

// agentVersion is reported to the panel in health responses. A var so release
// builds can stamp it with -ldflags -X main.agentVersion=...
var agentVersion = "0.1.0"

func main() {
	// `vortex-node version` answers a probe without starting the agent: the
	// upgrader runs the new binary once before it replaces the running one.
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("vortex-node %s\n", agentVersion)
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log); err != nil {
		log.Error("node agent exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("node agent shut down cleanly")
}

func run(ctx context.Context, log *slog.Logger) error {
	cfg, err := config.LoadNode()
	if err != nil {
		return err
	}

	// Make sure the proxy core the panel will drive actually exists. Installing
	// it is part of provisioning, not an operator chore.
	reports := agent.EnsureCores(ctx, cfg, log)

	// The binary decides whether the config may carry the per-user stats block.
	// Probing here means an upgrade to a stock build pauses accounting instead of
	// handing the core a config it refuses to start with.
	agent.ApplyCoreCapabilities(ctx, cfg, log)

	// Select the core driver this node runs. Both satisfy core.CoreDriver, so the
	// rest of the agent is engine-agnostic. The panel drives it through the
	// NodeServer; nothing starts until the first Sync.
	driver, err := agent.NewDriver(cfg, log)
	if err != nil {
		return err
	}
	defer driver.Stop(context.Background())

	decoySrv := decoy.NewServer()
	defer func() { _ = decoySrv.Stop(context.Background()) }()

	srv := vgrpc.NewNodeServer(driver, agentVersion, decoySrv)
	srv.SetAgentUpgrader(func(c context.Context, url, sha string) error {
		if err := agent.UpgradeSelf(c, url, sha, log); err != nil {
			return err
		}
		// Answer the RPC first, then become the new binary. systemd keeps
		// tracking the same PID through exec.
		go func() {
			time.Sleep(800 * time.Millisecond)
			exe, err := os.Executable()
			if err != nil {
				log.Error("cannot resolve own executable after upgrade", "err", err)
				return
			}
			log.Info("restarting upgraded agent", "path", exe)
			if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
				log.Error("upgraded agent failed to restart", "err", err)
			}
		}()
		return nil
	})
	srv.SetCoreInstaller(func(c context.Context, core, version, url, sha string, allowUnverified bool) (bool, error) {
		if err := agent.InstallCore(c, cfg, core, version, url, sha, allowUnverified, log); err != nil {
			return false, err
		}
		if !agent.MeteringCapabilityChanged(c, cfg) {
			return false, nil
		}
		// The stats block has to appear or disappear; that decision is baked into
		// the driver, so rebuild the process. Stop the core first: the new agent
		// will start it again from the panel's next sync.
		log.Info("core metering capability changed; restarting agent to rebuild the config",
			"core", core, "version", version)
		_ = driver.Stop(context.Background())
		go func() {
			time.Sleep(700 * time.Millisecond)
			exe, err := os.Executable()
			if err != nil {
				log.Error("cannot resolve own executable for restart", "err", err)
				return
			}
			log.Info("restarting agent", "path", exe)
			if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
				log.Error("agent restart failed; node keeps running the old config", "err", err)
			}
		}()
		return true, nil
	})
	for _, rep := range reports {
		if rep.Core == cfg.Core || len(reports) == 1 {
			srv.SetCoreReport(rep.Version, rep.Tags)
			break
		}
	}

	// A rotating identity: the panel can push a new certificate and the agent
	// swaps it in without a restart, so no SSH access is needed.
	reloader, err := vgrpc.NewCertReloader(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return err
	}
	srv.SetCertReloader(reloader)

	creds, err := vgrpc.ServerCredsReloadable(vgrpc.TLSFiles{CA: cfg.TLSCA}, reloader)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	// Serve in the background; stop gracefully when the signal context cancels.
	errCh := make(chan error, 1)
	go func() {
		log.Info("node agent listening (mTLS)", "addr", cfg.ListenAddr, "cores", cfg.EnabledCores)
		errCh <- srv.Serve(lis,
			grpc.Creds(creds),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second, // allow client pings every 10s
				PermitWithoutStream: true,
			}),
			grpc.KeepaliveParams(keepalive.ServerParameters{
				Time:    30 * time.Second, // server pings client every 30s
				Timeout: 10 * time.Second,
			}),
		)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down node agent")
		srv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}
