// Package routercmd is the body of the router command: the Go rewrite of the LiteLLM proxy: the OpenAI-compatible
// front door for the fleet. It reads models.yaml directly and routes each
// request to the right upstream — local node backend, tool proxy, or external
// API — streaming SSE through untouched. See docs/PLAN.md (Phase 3).
package routercmd

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/erewhon/llm-router-go/cli/internal/exitcode"
	"github.com/erewhon/llm-router-go/internal/auth"
	"github.com/erewhon/llm-router-go/internal/config"
	"github.com/erewhon/llm-router-go/internal/health"
	"github.com/erewhon/llm-router-go/internal/httpx"
	"github.com/erewhon/llm-router-go/internal/logx"
	"github.com/erewhon/llm-router-go/internal/router"
	"github.com/erewhon/llm-router-go/internal/router/reqlog"
)

// Version is what --version prints and what the server reports; the cmd/
// wrapper (or a host program) forwards its ldflags-stamped value here before
// calling Run.
var Version = "dev"

// defaultReqlogPath returns the zero-config SQLite request-log location:
// $XDG_STATE_HOME/llm-router/requests.db, falling back to ~/.local/state and
// then the OS temp dir if the home directory can't be determined.
func defaultReqlogPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".local", "state")
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "llm-router", "requests.db")
}

func Run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("router", flag.ContinueOnError)
	var (
		addr         = fs.String("addr", ":4010", "listen address")
		modelsYAML   = fs.String("models-yaml", "/etc/llm-router/models.yaml", "path to models.yaml")
		mode         = fs.String("mode", "", `mode tag filter ("big"/"default"/...); empty = all enabled models`)
		logLevel     = fs.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat    = fs.String("log-format", "json", "log format: json or text")
		shutdownTo   = fs.Duration("shutdown-timeout", 5*time.Second, "graceful shutdown deadline")
		postgresDSN  = fs.String("postgres-dsn", "", `Postgres DSN for request logging (e.g. "postgres://user:pw@host/db"); when set it takes precedence over SQLite`)
		sqlitePath   = fs.String("sqlite-path", "", "path to the SQLite request-log DB; empty uses $XDG_STATE_HOME/llm-router/requests.db. Ignored when --postgres-dsn is set")
		reqlogMode   = fs.String("reqlog", "auto", `request logging: "auto" (Postgres if --postgres-dsn, else a local SQLite file) or "off" to disable`)
		toolProxyURL = fs.String("tool-proxy-url", "", `address tool_proxy models route to; empty falls back to $ROUTER_TOOL_PROXY_URL, then the built-in default`)

		// Availability tracking — what lets a role follow the fleet as nodes
		// are powered down for the night. Off means roles always pick their
		// first candidate (the pre-roles behaviour).
		zdrCanary         = fs.Bool("zdr-canary", true, "periodically verify the OpenRouter account still enforces zero data retention account-wide; reported on /health as zdr_account")
		zdrCanaryInterval = fs.Duration("zdr-canary-interval", 6*time.Hour, "how often the ZDR canary probes account posture")

		healthTracking  = fs.Bool("health-tracking", true, "poll node agents so roles route to models that are actually up")
		healthInterval  = fs.Duration("health-interval", health.DefaultInterval, "how often to poll each node agent")
		healthDownAfter = fs.Int("health-down-after", health.DefaultDownAfter, "consecutive failed polls before a model is considered down (one success restores it)")
		breakerTrip     = fs.Int("breaker-trip", health.DefaultBreakerTrip, "consecutive upstream failures before a model's circuit breaker opens")
		breakerCooldown = fs.Duration("breaker-cooldown", health.DefaultBreakerCooldown, "how long an open circuit breaker waits before admitting a probe request")
		genProbe        = fs.Bool("generation-probe", true, "a fleet seat is routable only after one minimal generation succeeds against its backend, not merely when its listing is up (per-model override: health.generation_probe)")
		genProbeTimeout = fs.Duration("generation-probe-timeout", health.DefaultProbeTimeout, "how long one generation probe may take before the seat is still considered warming")
		// Live inventory: each upstream base's /v1/models, on its own
		// interval. Drift (a hand-written entry the base no longer lists) is
		// marked absent; ids a `discovery:` source adopts become routable.
		inventory         = fs.Bool("inventory", true, "poll every upstream base's /v1/models: an entry the base no longer lists is marked absent and dropped from /v1/models; ids adopted under models.yaml `discovery:` become routable (per-model override: health.inventory)")
		inventoryInterval = fs.Duration("inventory-interval", health.DefaultInventoryInterval, "how often each upstream base's listing is refreshed")

		// /.well-known/opencode (3b.iv). Empty -wellknown-provider-id disables.
		wellKnownProviderID   = fs.String("wellknown-provider-id", "", `provider key under "provider" in /.well-known/opencode (e.g. "llm"); empty disables the endpoint`)
		wellKnownProviderName = fs.String("wellknown-provider-name", "LLM Router", "human label OpenCode shows for the provider")
		wellKnownBaseURL      = fs.String("wellknown-base-url", "", "public OpenAI-compatible URL OpenCode hits (e.g. https://llm.bcc.sh/v1)")
		// No --wellknown-api-key any more (removed 2026-09-11): the document
		// never carries a credential. People mint a PAT on the dashboard and
		// `/connect` it in OpenCode; the auth command prints those steps.
		wellKnownSetupURL = fs.String("wellknown-setup-url", "", "where a person mints a personal access token, printed by the well-known's setup instructions (e.g. https://llm-dashboard.bcc.sh)")

		// API key auth. Empty list disables auth — anyone reachable can call
		// /v1/*. /health, /metrics, /.well-known/opencode are always exempt.
		// If --api-keys is empty, falls back to $ROUTER_API_KEYS (typical
		// pattern: load that env var from systemd's EnvironmentFile so the
		// key isn't visible in /proc/PID/cmdline).
		apiKeys = fs.String("api-keys", "", `comma-separated bearer tokens accepted on /v1/*; empty falls back to $ROUTER_API_KEYS, then disables auth`)

		// Dashboard: the status UI baked into the binary, served on its own
		// listener (separate auth boundary from /v1/*). Off by default so
		// existing deployments are unaffected; the loopback-default addr keeps
		// it safe to enable without auth on a local instance.
		dashboard     = fs.Bool("dashboard", false, "serve the status dashboard UI on --dashboard-addr")
		dashboardAddr = fs.String("dashboard-addr", "127.0.0.1:4011", "listen address for the dashboard; carries NO bearer auth, so keep it on loopback unless fronted by your own auth")
		dashboardURL  = fs.String("dashboard-public-url", "", "public OpenAI-compatible base URL shown in the dashboard's Connection card (e.g. https://llm.bcc.sh); empty derives http://localhost:<port> from --addr")

		// Personal access tokens. --pat-db points at the SQLite token store;
		// empty means PATs are off and only --api-keys shared keys are
		// accepted. Unlike --reqlog (which soft-fails to a NopSink), a
		// configured store that cannot be opened is FATAL: a router told to
		// check tokens must never quietly serve without checking them.
		patDB      = fs.String("pat-db", "", "path to the personal-access-token SQLite store; empty disables PATs (shared --api-keys only). Per-node — do NOT use behind a load balancer, see --pat-dsn")
		patDSN     = fs.String("pat-dsn", "", "Postgres DSN for the personal-access-token store, e.g. the reqlog DSN. Takes precedence over --pat-db. REQUIRED for multi-instance deployments: a per-node store means a token minted on one replica is unknown to the others")
		patMint    = fs.Bool("pat-mint", false, "mint a PAT into --pat-db and exit; requires --pat-user")
		patList    = fs.Bool("pat-list", false, "list PATs in --pat-db and exit")
		patRevoke  = fs.String("pat-revoke", "", "revoke the PAT with this id in --pat-db and exit")
		patUser    = fs.String("pat-user", "", "with --pat-mint: the principal the token belongs to; with --pat-list: filter to one principal")
		patLabel   = fs.String("pat-label", "", "with --pat-mint: a human label for the token (e.g. 'laptop', 'background-agent')")
		patScope   = fs.String("pat-scope", "", "with --pat-mint: models scope — models:local (fleet hardware only), models:local_or_zdr, or models:* (default, unrestricted)")
		patExpires = fs.Duration("pat-expires", 0, "with --pat-mint: lifetime (e.g. 720h); zero means no expiry")

		// Dashboard identity. The dashboard listener has no auth of its own:
		// behind the hub it is oauth2-proxy-gated and Caddy forwards the
		// resolved identity in X-Auth-Request-*. Nothing proves those headers
		// came from Caddy rather than from whoever can reach the VIP, so the
		// identity-bearing routes (/api/tokens, /api/chat, /api/usage) also
		// require a shared secret only Caddy knows. Empty = self-service off.
		dashAuthSecret = fs.String("dashboard-auth-secret", "", "shared secret the front proxy sends in X-Dashboard-Auth; gates the dashboard's identity-bearing routes. Empty falls back to $DASHBOARD_AUTH_SECRET, then disables token self-service")
		dashOwners     = fs.String("dashboard-owners", "", "comma-separated principals allowed to mint unrestricted (models:*) tokens from the dashboard; everyone else is capped at models:local. Empty falls back to $DASHBOARD_OWNERS")
		dashIDHeader   = fs.String("dashboard-identity-header", "X-Auth-Request-Email", "request header carrying the proxy-verified principal")
		// Agents tab: an agent-monitor web UI the dashboard's browser can
		// reach. Meant for a router run on the same machine as its agents (a
		// laptop); `pitf router serve` exports PITF_MONITOR_URL, so it wires
		// itself there.
		dashMonitorURL = fs.String("dashboard-monitor-url", "", "agent-monitor web UI (e.g. http://127.0.0.1:8070) for the dashboard's Agents tab; empty falls back to $PITF_MONITOR_URL, then hides the tab")

		// Anthropic passthrough attribution: the operator's map from an
		// upstream credential's fingerprint (the 8 hex chars after
		// "anthropic:" in reqlog) to a person.
		anthropicKeyPrincipals = fs.String("anthropic-key-principals", "", "comma-separated <fingerprint>=<principal> pairs mapping Anthropic credential fingerprints to people for /v1/messages attribution. Empty falls back to $ANTHROPIC_KEY_PRINCIPALS")

		showVer = fs.Bool("version", false, "print version and exit")

		// --validate: check --models-yaml and exit without serving. See
		// validate.go for the exit-code contract. Lets a deploy validate an
		// edit BEFORE restarting anything, instead of discovering a bad
		// config by watching production fail to come back.
		validate       = fs.Bool("validate", false, "validate --models-yaml and exit; does not serve")
		validateFormat = fs.String("validate-format", "text", "with --validate: output format, text or json")
		validateModes  = fs.String("validate-mode", "default", "with --validate: comma-separated modes to lint (every mode:<tag> the file uses, plus default)")
		validateStrict = fs.Bool("validate-strict", false, "with --validate: treat every lint warning as a failure")
		validateBlock  = fs.String("validate-block", "", "with --validate: comma-separated lint codes promoted to failures (e.g. enabled-port-collision)")
		validateLive   = fs.Bool("validate-live", false, "with --validate: also fetch every upstream base's /v1/models and report hand-written entries the provider no longer lists (not-listed), unreachable bases, and ids a discovery source would adopt (discovered-model, informational)")
	)
	if err := fs.Parse(args); err != nil {
		return exitcode.Parse(err)
	}
	if *showVer {
		fmt.Println(Version)
		return nil
	}
	// Dispatch BEFORE logx.New: the logger writes JSON to stdout, which would
	// interleave with and corrupt --validate-format=json.
	if *validate {
		if f := *validateFormat; f != "text" && f != "json" {
			fmt.Fprintf(os.Stderr, "unknown --validate-format %q (want text or json)\n", f)
			return exitcode.Status(2)
		}
		return exitcode.Status(runValidate(validateOpts{
			path:   *modelsYAML,
			format: *validateFormat,
			modes:  parseModes(*validateModes),
			strict: *validateStrict,
			block:  parseBlockList(*validateBlock),
			live:   *validateLive,
			stdout: os.Stdout,
			stderr: os.Stderr,
		}))
	}
	// PAT admin modes, dispatched here for the same reason as --validate:
	// they print to stdout and exit without serving.
	if *patMint || *patList || *patRevoke != "" {
		return exitcode.Status(runPATAdmin(patAdminOpts{
			dbPath:  *patDB,
			dsn:     *patDSN,
			mint:    *patMint,
			list:    *patList,
			revoke:  *patRevoke,
			user:    *patUser,
			label:   *patLabel,
			scope:   *patScope,
			expires: *patExpires,
			stdout:  os.Stdout,
			stderr:  os.Stderr,
		}))
	}

	level, err := logx.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitcode.Status(2)
	}
	logger := logx.New(os.Stdout, logx.Config{
		Level:  level,
		Format: *logFormat,
		Attrs:  httpx.LogAttrsFromContext,
	}).With("svc", "router")

	registry, err := config.Load(*modelsYAML)
	if err != nil {
		logger.Error("load registry failed", "path", *modelsYAML, "err", err)
		return exitcode.Status(1)
	}

	// Env-var fallback for secrets. Standard pattern: systemd ships them
	// via EnvironmentFile so they don't appear in /proc/PID/cmdline. Flags
	// still win if set, so dev/test runs are unaffected.
	if os.Getenv("WELLKNOWN_API_KEY") != "" {
		logger.Warn("WELLKNOWN_API_KEY is set but no longer read: /.well-known/opencode stopped carrying a shared key on 2026-09-11; remove it from proxy.env")
	}
	if *toolProxyURL == "" {
		*toolProxyURL = os.Getenv("ROUTER_TOOL_PROXY_URL")
	}
	if *toolProxyURL != "" {
		registry.ToolProxyAddr = *toolProxyURL
		logger.Info("tool proxy address overridden", "addr", *toolProxyURL)
	}

	routerOpts := []router.Option{
		router.WithMode(*mode),
		router.WithVersion(Version),
		router.WithWellKnown(router.WellKnownConfig{
			ProviderID:   *wellKnownProviderID,
			ProviderName: *wellKnownProviderName,
			BaseURL:      *wellKnownBaseURL,
			SetupURL:     *wellKnownSetupURL,
		}),
	}
	// Request logging. Precedence: --reqlog=off disables entirely; otherwise
	// --postgres-dsn (if set) wins, else a local SQLite file (the zero-config
	// default). Any sink open failure soft-fails to NopSink — a request-log
	// outage must never take down the proxy — logged loudly so the gap shows
	// in the journal.
	var sink reqlog.Sink = reqlog.NopSink{}
	switch strings.ToLower(*reqlogMode) {
	case "off", "none", "disable", "disabled":
		logger.Info("reqlog disabled (--reqlog=off); requests will NOT be logged")
	case "auto", "":
		if *postgresDSN != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			ps, err := reqlog.NewPostgres(ctx, *postgresDSN, logger.With("subsys", "reqlog"))
			cancel()
			if err != nil {
				logger.Warn("reqlog postgres unavailable; falling back to NopSink (requests will NOT be logged)",
					"err", err, "dsn", reqlog.RedactDSN(*postgresDSN))
			} else {
				sink = ps
				defer ps.Close()
				logger.Info("reqlog enabled", "backend", "postgres", "dsn", reqlog.RedactDSN(*postgresDSN))
			}
		} else {
			path := *sqlitePath
			if path == "" {
				path = defaultReqlogPath()
			}
			ss, err := reqlog.NewSQLite(path, logger.With("subsys", "reqlog"))
			if err != nil {
				logger.Warn("reqlog sqlite unavailable; falling back to NopSink (requests will NOT be logged)",
					"err", err, "path", path)
			} else {
				sink = ss
				defer ss.Close()
				logger.Info("reqlog enabled", "backend", "sqlite", "path", path)
			}
		}
	default:
		logger.Warn("unknown --reqlog value; request logging disabled", "value", *reqlogMode)
	}
	routerOpts = append(routerOpts, router.WithSink(sink))

	// Availability tracking: the poller that lets roles follow the fleet as
	// nodes power down. Disabling it leaves roles resolving to their first
	// candidate unconditionally, i.e. the pre-roles behaviour.
	var tracker *health.Tracker
	if *healthTracking {
		if errs := health.ValidateSchedules(registry); len(errs) > 0 {
			// A typo'd window is not fatal (schedules are cosmetic) but it
			// silently disables the badge, so say so loudly.
			for _, e := range errs {
				logger.Warn("node schedule ignored", "err", e)
			}
		}
		// Probe only what this router can route to: an out-of-mode seat's
		// port is expected to be dead and must not sit in "warming" forever.
		active := registry.ModelsForMode(*mode)
		tracker = health.NewTracker(health.Config{
			Registry:               registry,
			Interval:               *healthInterval,
			DownAfter:              *healthDownAfter,
			BreakerTrip:            *breakerTrip,
			BreakerCooldown:        *breakerCooldown,
			DisableGenerationProbe: !*genProbe,
			ProbeTimeout:           *genProbeTimeout,
			DisableInventory:       !*inventory,
			InventoryInterval:      *inventoryInterval,
			ProbeFilter:            func(id string) bool { _, ok := active[id]; return ok },
			Logger:                 logger.With("subsys", "availability"),
		})
		routerOpts = append(routerOpts, router.WithAvailability(tracker))
	}

	var canary *router.ZDRCanary
	if *zdrCanary {
		if canary = buildZDRCanary(registry, *zdrCanaryInterval, logger); canary != nil {
			routerOpts = append(routerOpts, router.WithZDRCanary(canary))
		} else {
			logger.Info("ZDR canary idle: no OpenRouter model in models.yaml with a resolvable api_key")
		}
	}

	rt := router.New(registry, logger, routerOpts...)
	if tracker != nil {
		// Republish the availability gauges after each poll, so /metrics
		// tracks fleet state rather than request traffic. Set after New
		// because the callback closes over the router.
		tracker.SetOnPoll(rt.PublishAvailabilityMetrics)
	}

	apiKeysSrc := *apiKeys
	apiKeysFrom := "flag"
	if apiKeysSrc == "" {
		if v := os.Getenv("ROUTER_API_KEYS"); v != "" {
			apiKeysSrc = v
			apiKeysFrom = "env"
		}
	}
	authKeys := splitCSV(apiKeysSrc)

	// Token store. FATAL on failure, unlike every other optional subsystem
	// here: asking for a token store is an explicit instruction to
	// authenticate callers, and falling back to "serve anyway" would silently
	// drop the control the operator asked for. reqlog soft-fails because
	// losing accounting is not dangerous; losing authentication is.
	//
	// --pat-dsn (Postgres, shared) wins over --pat-db (SQLite, per-node), the
	// same precedence reqlog gives --postgres-dsn over --sqlite-path. Passing
	// both is a config mistake worth naming rather than resolving silently:
	// the two stores hold different tokens, so picking one quietly would make
	// half the fleet's credentials vanish depending on flag order.
	var patStore *auth.Store
	switch {
	case *patDSN != "" && *patDB != "":
		fmt.Fprintln(os.Stderr, "fatal: --pat-dsn and --pat-db are mutually exclusive; they are different stores holding different tokens")
		return exitcode.Status(2)
	case *patDSN != "":
		st, err := auth.OpenPostgresStore(*patDSN)
		if err != nil {
			// Redacted: the DSN carries a password and this goes to the journal.
			fmt.Fprintf(os.Stderr, "fatal: --pat-dsn %s: %v\n", auth.RedactDSN(*patDSN), err)
			return exitcode.Status(1)
		}
		defer st.Close()
		patStore = st
		logger.Info("PAT auth enabled", "store", st.Path(), "backend", st.Backend())
	case *patDB != "":
		st, err := auth.OpenStore(*patDB)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: --pat-db %s: %v\n", *patDB, err)
			return exitcode.Status(1)
		}
		defer st.Close()
		patStore = st
		// Say the quiet part at startup rather than leaving it to a comment in
		// a unit file: a per-node store behind a load balancer authenticates
		// intermittently, which is the hardest kind of bug to recognise.
		logger.Info("PAT auth enabled", "store", st.Path(), "backend", st.Backend())
		logger.Warn("PAT store is PER-NODE (SQLite) — correct for a single router, WRONG behind a load balancer: tokens minted here are unknown to other instances. Use --pat-dsn for a shared store")
	}

	switch {
	case patStore == nil && len(authKeys) == 0:
		logger.Warn("API key auth DISABLED — anyone reachable on :4010 can call the proxy; set --api-keys, $ROUTER_API_KEYS, --pat-dsn or --pat-db to enable")
	case len(authKeys) > 0:
		logger.Info("shared-key auth enabled", "keys", len(authKeys), "source", apiKeysFrom,
			"note", "legacy shared keys are attributed as legacy:<fingerprint> in reqlog")
	}
	// /health, /metrics, /.well-known/opencode are always exempt. The Anthropic
	// passthrough carries the caller's own credentials, which the router
	// forwards untouched, so it is never gated — but it IS attributed (see
	// Authenticator.attributePassthrough).
	authExempt := []string{"/health", "/metrics", "/.well-known/opencode"}
	keyPrincipalsSrc := *anthropicKeyPrincipals
	if keyPrincipalsSrc == "" {
		keyPrincipalsSrc = os.Getenv("ANTHROPIC_KEY_PRINCIPALS")
	}
	keyPrincipals := map[string]string{}
	for _, pair := range splitCSV(keyPrincipalsSrc) {
		fp, principal, ok := strings.Cut(pair, "=")
		fp, principal = strings.TrimSpace(fp), strings.TrimSpace(principal)
		if !ok || fp == "" || principal == "" {
			fmt.Fprintf(os.Stderr, "fatal: --anthropic-key-principals: %q is not <fingerprint>=<principal>\n", pair)
			return exitcode.Status(2)
		}
		keyPrincipals[fp] = principal
	}
	if len(keyPrincipals) > 0 {
		logger.Info("anthropic passthrough attribution: fingerprint map loaded", "entries", len(keyPrincipals))
	}

	handler := httpx.Chain(
		rt.Handler(),
		httpx.RequestID,
		httpx.AccessLog(logger),
		httpx.Recover(logger),
		router.NewAuthenticator(patStore, authKeys, authExempt, logger).
			Passthrough(router.AnthropicPaths...).
			KeyPrincipals(keyPrincipals).
			Middleware(),
	)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if canary != nil {
		go canary.Run(ctx)
		logger.Info("ZDR canary started", "interval", zdrCanaryInterval.String())
	}

	if tracker != nil {
		go tracker.Run(ctx)
		logger.Info("availability tracking started",
			"interval", healthInterval.String(), "down_after", *healthDownAfter,
			"breaker_trip", *breakerTrip, "breaker_cooldown", breakerCooldown.String(),
			"generation_probe", *genProbe, "generation_probe_timeout", genProbeTimeout.String(),
			"inventory", *inventory, "inventory_interval", inventoryInterval.String())
	} else {
		logger.Warn("availability tracking disabled; roles will always pick their first candidate")
	}

	if *dashboard {
		apiBase := *dashboardURL
		if apiBase == "" {
			apiBase = deriveAPIBase(*addr)
		}
		secret := *dashAuthSecret
		if secret == "" {
			secret = os.Getenv("DASHBOARD_AUTH_SECRET")
		}
		owners := *dashOwners
		if owners == "" {
			owners = os.Getenv("DASHBOARD_OWNERS")
		}
		switch {
		case !isLoopbackBind(*dashboardAddr) && secret == "":
			logger.Warn("dashboard listener is NOT loopback and no --dashboard-auth-secret is set — it has no bearer auth and its /api/chat can invoke any model; front it with your own auth (oauth2-proxy) or bind 127.0.0.1. Token self-service is OFF",
				"addr", *dashboardAddr)
		case secret != "" && patStore == nil:
			logger.Warn("dashboard identity is configured but no PAT store is — /api/chat and /api/usage are gated, token self-service is OFF")
		case secret != "":
			logger.Info("dashboard identity enabled: token self-service on", "identity_header", *dashIDHeader, "owners", len(splitCSV(owners)))
		}
		dashHandler := httpx.Chain(
			rt.DashboardHandler(router.DashboardConfig{
				APIBase:        apiBase,
				ProviderID:     *wellKnownProviderID,
				SetupHint:      router.SetupInstructions(*wellKnownProviderID, *wellKnownSetupURL, "LLM_ROUTER_API_KEY"),
				Tokens:         patStore,
				AuthSecret:     secret,
				IdentityHeader: *dashIDHeader,
				Owners:         splitCSV(owners),
				MonitorURL:     monitorURL(*dashMonitorURL),
			}),
			httpx.RequestID,
			httpx.AccessLog(logger.With("svc", "dashboard")),
			httpx.Recover(logger),
		)
		dashSrv := &http.Server{
			Addr:              *dashboardAddr,
			Handler:           dashHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			logger.Info("dashboard starting", "addr", *dashboardAddr, "public_url", apiBase)
			// Soft-fail, like the reqlog sink: the dashboard is auxiliary, so a
			// bind error (e.g. the old service still holds the port mid-rollout)
			// is logged loudly but must never take down inference on --addr.
			if err := httpx.ServeContext(ctx, dashSrv, *shutdownTo); err != nil {
				logger.Error("dashboard listener stopped with error; router continues without it", "err", err)
			}
		}()
	}

	logger.Info("starting", "addr", *addr, "version", Version,
		"models_yaml", *modelsYAML, "mode", *mode,
		"models", len(registry.ModelsForMode(*mode)))
	if err := httpx.ServeContext(ctx, srv, *shutdownTo); err != nil {
		logger.Error("server stopped with error", "err", err)
		return exitcode.Status(1)
	}
	logger.Info("shutdown complete")
	return nil
}

// deriveAPIBase turns a listen address (":4010", "0.0.0.0:4010",
// "192.168.42.240:4010") into the base URL a local client would hit. A
// wildcard or empty host collapses to localhost — the dashboard's Connection
// card is a copy-paste hint, and "http://0.0.0.0:4010" isn't dialable.
func deriveAPIBase(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://localhost:4010"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// isLoopbackBind reports whether addr binds only the loopback interface. An
// empty or wildcard host ("":4011, "0.0.0.0:4011") is NOT loopback — it's
// reachable from the network, so the dashboard's no-auth warning fires.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildZDRCanary picks a probe target for the account-posture canary out of
// models.yaml, returning nil when there is nothing to watch.
//
// The target is discovered rather than configured because a flag would be one
// more thing to keep in sync with the registry, and a stale flag pointing at a
// retired model would make the canary report "unknown" forever — the failure
// mode a compliance check can least afford, since it looks like silence rather
// than like breakage.
//
// Selection: a ZDR-enforceable (OpenRouter) chat model whose api_key resolves,
// preferring an Anthropic-backed one because zdrCanaryProvider pins the
// Anthropic endpoint and the probe must name a model that endpoint actually
// serves. Ids are sorted first so the choice is deterministic across restarts
// — an unstable target would make the /health detail string change for no
// reason and cost someone an afternoon.
func buildZDRCanary(registry *config.ModelRegistry, interval time.Duration, logger *slog.Logger) *router.ZDRCanary {
	ids := make([]string, 0, len(registry.Models))
	for id := range registry.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var fallbackID string
	for _, id := range ids {
		m := registry.Models[id]
		if !m.Enabled || m.APIClass != config.APIClassChat || !m.ZDREnforceable() {
			continue
		}
		if os.Getenv(m.APIKey) == "" {
			continue
		}
		if strings.Contains(strings.ToLower(m.HFRepo), "anthropic/") {
			return router.NewZDRCanary(m.APIBase, os.Getenv(m.APIKey), m.HFRepo, interval, logger)
		}
		if fallbackID == "" {
			fallbackID = id
		}
	}
	if fallbackID == "" {
		return nil
	}
	// No Anthropic-backed entry. Probing a non-Anthropic model pinned to the
	// Anthropic provider yields "no endpoints" rather than a ZDR verdict, so
	// say plainly that the canary will be inconclusive instead of letting it
	// look healthy.
	m := registry.Models[fallbackID]
	logger.Warn("ZDR canary has no Anthropic-backed OpenRouter model to probe; verdicts will likely be inconclusive",
		"using", fallbackID)
	return router.NewZDRCanary(m.APIBase, os.Getenv(m.APIKey), m.HFRepo, interval, logger)
}

// monitorURL is the Agents tab's agent-monitor base: the flag, else
// $PITF_MONITOR_URL (what pitf exports to the commands it mounts).
func monitorURL(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv("PITF_MONITOR_URL")
}
