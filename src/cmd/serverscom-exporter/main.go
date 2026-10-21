// Command serverscom-exporter exposes Servers.com Enterprise Bare Metal
// inventory and state (servers, power, drives, ports, L2 segments, uplink
// aggregation) as Prometheus metrics.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	promslogflag "github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/collector"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/refresher"
)

const exporterName = "serverscom_exporter"

func main() {
	var (
		apiURL = kingpin.Flag("api.url", "Servers.com API base URL.").
			Default(api.DefaultBaseURL).Envar("SERVERSCOM_API_URL").String()
		apiToken = kingpin.Flag("api.token", "Servers.com API token. Prefer --api.token-file or the environment variable.").
				Envar("SERVERSCOM_API_TOKEN").String()
		apiTokenFile = kingpin.Flag("api.token-file", "File containing the Servers.com API token.").
				Envar("SERVERSCOM_API_TOKEN_FILE").String()
		apiTimeout = kingpin.Flag("api.timeout", "Timeout of a single API request.").
				Default("30s").Duration()
		apiPageSize = kingpin.Flag("api.page-size", "Page size (per_page) for collection endpoints.").
				Default("100").Int()
		apiBudget = kingpin.Flag("api.requests-per-hour", "Hourly request budget of this exporter. Requests are not delayed until it is used up; then the exporter waits for the hourly reset. The account limit is 2000/h and is shared with other API users.").
				Default("1000").Int()
		apiReserve = kingpin.Flag("api.ratelimit-reserve", "Pause when the account-wide X-RateLimit-Remaining drops to this value, leaving the rest to other API users.").
				Default("300").Int()

		hostsInterval = kingpin.Flag("refresh.hosts-interval", "How often to fetch the server list (power, statuses, labels).").
				Default("2m").Duration()
		detailsInterval = kingpin.Flag("refresh.details-interval", "Maximum age of per-server details (drives, ports, networks, features, power feeds). Changed servers are refreshed earlier.").
				Default("12h").Duration()
		featuresInterval = kingpin.Flag("refresh.features-interval", "How often to re-fetch server features (port aggregation, OOB public access) for all servers. Toggling a feature does not change updated_at. Costs one request per server; 0 disables.").
					Default("1h").Duration()
		l2Interval = kingpin.Flag("refresh.l2-interval", "How often to fetch L2 segments with members and networks.").
				Default("15m").Duration()
		referenceInterval = kingpin.Flag("refresh.reference-interval", "How often to fetch locations, racks and uplink models.").
					Default("24h").Duration()

		cacheFile = kingpin.Flag("cache.file", "File to persist the cache to, so restarts do not refetch everything. Empty disables persistence.").
				Default("").String()
		cacheSaveInterval = kingpin.Flag("cache.save-interval", "How often to write the cache file if it changed.").
					Default("1m").Duration()

		hostLabels = kingpin.Flag("collector.host-labels", "Comma-separated Servers.com host label keys to expose in serverscom_host_labels; '*' for all.").
				Default("*").String()
		metricsPath = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").
				Default("/metrics").String()
		toolkitFlags = webflag.AddFlags(kingpin.CommandLine, ":9920")
	)

	logConfig := &promslog.Config{}
	promslogflag.AddFlags(kingpin.CommandLine, logConfig)
	kingpin.Version(version.Print(exporterName))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := promslog.New(logConfig)
	logger.Info("Starting "+exporterName, "version", version.Info(), "build_context", version.BuildContext())

	token, err := readToken(*apiToken, *apiTokenFile)
	if err != nil {
		logger.Error("Cannot read API token", "err", err)
		os.Exit(1)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		versioncollector.NewCollector(exporterName),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	client, err := api.NewClient(api.Options{
		BaseURL:       *apiURL,
		Token:         token,
		UserAgent:     exporterName + "/" + version.Version,
		Timeout:       *apiTimeout,
		PageSize:      *apiPageSize,
		BudgetPerHour: *apiBudget,
		Reserve:       *apiReserve,
		Logger:        logger.With("component", "api"),
		Registerer:    registry,
	})
	if err != nil {
		logger.Error("Cannot create API client", "err", err)
		os.Exit(1)
	}

	logger.Info("Configuration",
		"api_url", *apiURL, "requests_per_hour", *apiBudget,
		"ratelimit_reserve", *apiReserve, "cache_file", *cacheFile)

	store := cache.NewStore(*cacheFile, logger.With("component", "cache"))
	store.Load()

	registry.MustRegister(collector.New(store, strings.Split(*hostLabels, ",")))

	ref := refresher.New(client, store, refresher.Config{
		HostsInterval:     *hostsInterval,
		DetailsInterval:   *detailsInterval,
		FeaturesInterval:  *featuresInterval,
		L2Interval:        *l2Interval,
		ReferenceInterval: *referenceInterval,
		PageSize:          *apiPageSize,
		BudgetPerHour:     *apiBudget,
		Reserve:           *apiReserve,
	}, logger.With("component", "refresher"), registry)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go ref.Run(ctx)

	persisterDone := make(chan struct{})
	go func() {
		defer close(persisterDone)
		store.RunPersister(ctx, *cacheSaveInterval)
	}()

	server, err := newHTTPServer(registry, *metricsPath, logger)
	if err != nil {
		logger.Error("Cannot set up HTTP server", "err", err)
		os.Exit(1)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- web.ListenAndServe(server, toolkitFlags, logger) }()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "err", err)
			stop()
			<-persisterDone
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("Shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Warn("HTTP server shutdown failed", "err", err)
		}
	}

	<-persisterDone // the persister saves the cache one last time on exit
}

func newHTTPServer(registry *prometheus.Registry, metricsPath string, logger *slog.Logger) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.Handle(metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandling:     promhttp.ContinueOnError,
		EnableOpenMetrics: true,
	}))
	mux.HandleFunc("/-/healthy", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})

	if metricsPath != "/" {
		landing, err := web.NewLandingPage(web.LandingConfig{
			Name:        "Servers.com Exporter",
			Description: "Prometheus exporter for the Servers.com API",
			Version:     version.Info(),
			Links: []web.LandingLinks{
				{Address: metricsPath, Text: "Metrics"},
				{Address: "/-/healthy", Text: "Health"},
			},
		})
		if err != nil {
			return nil, err
		}
		mux.Handle("/", landing)
	}

	return &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}, nil
}

// readToken returns the token from the flag/environment or from a file.
func readToken(token, file string) (string, error) {
	if token != "" && file != "" {
		return "", errors.New("set only one of --api.token and --api.token-file")
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		return "", fmt.Errorf("no token: set --api.token-file, SERVERSCOM_API_TOKEN or --api.token")
	}
	return token, nil
}
