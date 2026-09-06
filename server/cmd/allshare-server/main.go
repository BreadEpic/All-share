// Command allshare-server is the ALL SHARE rendezvous.
//
// It is one static binary with no external dependencies. It performs
// signalling, presence, pairing brokerage and wake orchestration, and can run
// an embedded TURN relay. It never sees screen contents, keystrokes or audio:
// those travel directly between the two devices under DTLS-SRTP keyed by a
// fingerprint that both peers verify against a pinned identity key.
//
// A personal deployment fits comfortably on the smallest instance any provider
// sells. Signalling traffic is a few kilobytes per session; only relayed
// sessions cost real bandwidth, and those are the minority.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mmc/all-share/internal/rendezvous/pairing"
	"github.com/mmc/all-share/internal/rendezvous/registry"
	allsignal "github.com/mmc/all-share/internal/rendezvous/signal"
	"github.com/mmc/all-share/internal/rendezvous/turnsvc"
	"github.com/mmc/all-share/shared/idkey"
	"github.com/mmc/all-share/shared/protocol"
)

// Version is stamped at build time with -ldflags "-X main.Version=…".
var Version = "dev"

type config struct {
	listen      string
	dataDir     string
	realm       string
	turnListen  string
	turnPublic  string
	turnSecret  string
	turnTTL     time.Duration
	stunOnly    bool
	trustProxy  bool
	localWOL    bool
	logLevel    string
	logJSON     bool
	extraICE    string
	showVersion bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "allshare-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.listen, "listen", envOr("ALLSHARE_LISTEN", ":8443"), "HTTP listen address for signalling")
	flag.StringVar(&cfg.dataDir, "data", envOr("ALLSHARE_DATA", "./data"), "directory for the device registry and server key")
	flag.StringVar(&cfg.realm, "realm", envOr("ALLSHARE_REALM", "allshare"), "TURN realm")
	flag.StringVar(&cfg.turnListen, "turn-listen", envOr("ALLSHARE_TURN_LISTEN", ""), "UDP address for the embedded relay, e.g. :3478 (empty disables it)")
	flag.StringVar(&cfg.turnPublic, "turn-public-ip", envOr("ALLSHARE_TURN_PUBLIC_IP", ""), "public IPv4 address peers can reach the relay on")
	flag.StringVar(&cfg.turnSecret, "turn-secret", envOr("ALLSHARE_TURN_SECRET", ""), "shared secret for ephemeral relay credentials")
	flag.DurationVar(&cfg.turnTTL, "turn-ttl", 12*time.Hour, "lifetime of a minted relay credential")
	flag.BoolVar(&cfg.stunOnly, "stun-only", envOr("ALLSHARE_STUN_ONLY", "") == "1", "advertise public STUN only and run no relay")
	flag.BoolVar(&cfg.trustProxy, "trust-proxy", envOr("ALLSHARE_TRUST_PROXY", "") == "1", "trust X-Forwarded-For (only behind a proxy you control)")
	flag.BoolVar(&cfg.localWOL, "local-wol", envOr("ALLSHARE_LOCAL_WOL", "") == "1", "let this server broadcast wake packets on its own network")
	flag.StringVar(&cfg.extraICE, "ice-servers", envOr("ALLSHARE_ICE_SERVERS", ""), "additional ICE servers as JSON")
	flag.StringVar(&cfg.logLevel, "log-level", envOr("ALLSHARE_LOG_LEVEL", "info"), "debug, info, warn or error")
	flag.BoolVar(&cfg.logJSON, "log-json", envOr("ALLSHARE_LOG_JSON", "") == "1", "emit structured JSON logs")
	flag.BoolVar(&cfg.showVersion, "version", false, "print the version and exit")
	flag.Parse()

	if cfg.showVersion {
		fmt.Println("allshare-server", Version)
		return nil
	}

	log := newLogger(cfg.logLevel, cfg.logJSON)
	slog.SetDefault(log)

	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	// The server's own identity gives it a stable name that peers bind their
	// authentication signature to, so a challenge answered for this server
	// cannot be replayed against a different one.
	serverKey, created, err := idkey.LoadOrCreate(filepath.Join(cfg.dataDir, "server-identity.key"))
	if err != nil {
		return fmt.Errorf("load server identity: %w", err)
	}
	if created {
		log.Info("generated a new server identity", "fingerprint", serverKey.Public().Fingerprint())
	}
	serverID := serverKey.Public().String()

	lanSalt, err := loadOrCreateSecret(filepath.Join(cfg.dataDir, "lan-key.secret"))
	if err != nil {
		return fmt.Errorf("load LAN key secret: %w", err)
	}

	reg, err := registry.Open(filepath.Join(cfg.dataDir, "devices.json"))
	if err != nil {
		return fmt.Errorf("open device registry: %w", err)
	}
	defer reg.Close()

	extraICE, err := parseExtraICE(cfg.extraICE)
	if err != nil {
		return fmt.Errorf("parse -ice-servers: %w", err)
	}

	turnSecret := []byte(cfg.turnSecret)
	if cfg.turnListen != "" && len(turnSecret) == 0 {
		turnSecret, err = loadOrCreateSecret(filepath.Join(cfg.dataDir, "turn.secret"))
		if err != nil {
			return fmt.Errorf("load relay secret: %w", err)
		}
		log.Info("generated a relay secret", "path", filepath.Join(cfg.dataDir, "turn.secret"))
	}

	relay, err := turnsvc.New(turnsvc.Config{
		PublicIP:     cfg.turnPublic,
		ListenAddr:   cfg.turnListen,
		Realm:        cfg.realm,
		Secret:       turnSecret,
		TTL:          cfg.turnTTL,
		ExternalURLs: extraICE,
		STUNOnly:     cfg.stunOnly || cfg.turnListen == "",
		Log:          log,
	})
	if err != nil {
		return fmt.Errorf("start relay: %w", err)
	}
	defer relay.Close()

	hubCfg := allsignal.Config{
		Log:        log,
		Registry:   reg,
		Pairs:      pairing.NewStore(),
		ICE:        relay,
		ServerID:   serverID,
		Version:    Version,
		TrustProxy: cfg.trustProxy,
		Limits:     allsignal.DefaultLimits(),
		LANKeySalt: lanSalt,
	}
	hub := allsignal.NewHub(hubCfg)

	if cfg.localWOL {
		// Only meaningful when the rendezvous is self-hosted on the same
		// network as the PCs, which is why it is opt-in rather than automatic.
		if ip := outboundIP(); ip != "" {
			hub.SetWOL(&allsignal.LocalWOL{LANKey: hub.LANKeyFor(ip)})
			log.Info("local wake-on-LAN enabled", "observedAddress", ip)
		} else {
			log.Warn("local wake-on-LAN requested but no outbound address could be determined")
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rv", hub.Serve)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"version": Version,
			"server":  serverKey.Public().Fingerprint(),
			"stats":   hub.Stats(),
		})
	})
	// The client is served from a local file, so its requests carry Origin:
	// null. Allowing any origin is safe because nothing here is reachable
	// without an Ed25519 signature and no cookies are ever set.
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":  Version,
			"serverId": serverID,
			"realm":    cfg.realm,
		})
	})

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           withSecurityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signalContext()
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("rendezvous listening",
			"addr", cfg.listen, "version", Version, "server", serverKey.Public().Fingerprint())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown did not finish cleanly", "err", err)
	}
	return reg.Flush()
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func newLogger(level string, asJSON bool) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if asJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// loadOrCreateSecret returns a 32-byte secret, generating one on first use.
// Stability matters: regenerating the LAN key secret would silently break
// peer-assisted wake, and regenerating the relay secret invalidates every
// outstanding credential.
func loadOrCreateSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		secret, err := idkey.DecodeB64(strings.TrimSpace(string(data)))
		if err == nil && len(secret) >= 32 {
			return secret, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(idkey.EncodeB64(secret)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return secret, nil
}

func parseExtraICE(s string) ([]protocol.ICEServer, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []protocol.ICEServer
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// outboundIP discovers the address this host uses to reach the internet,
// without actually sending anything.
func outboundIP() string {
	conn, err := net.Dial("udp4", "203.0.113.1:9")
	if err != nil {
		return ""
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return host
}
