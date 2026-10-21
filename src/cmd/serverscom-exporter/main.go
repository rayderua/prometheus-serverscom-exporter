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

// maxPageSize is the largest per_page the Servers.com API accepts.
const maxPageSize = 100

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
		apiPageSize = kingpin.Flag("api.page-size", "Page size (per_page) for collection endpoints, 1-100 (the API maximum).").
				Default("100").Int()
		apiBudget = kingpin.Flag("api.requests-per-hour", "Hourly request budget of this exporter. Requests are not delayed until it is used up; then the exporter waits for the hourly reset. The account limit is 2000/h and is shared with other API users.").
				Default("1000").Int()
		apiReserve = kingpin.Flag("api.ratelimit-reserve", "Pause when the account-wide X-RateLimit-Remaining drops to this value, leaving the rest to other API users.").
				Default("300").Int()

		hostsInterval = kingpin.Flag("refresh.hosts-interval", "How often to fetch the server list (power, statuses, labels) and download new and changed servers.").
				Default("2m").Duration()
		autoMinInterval = kingpin.Flag("refresh.auto-min-interval", "Lower bound of the auto refresh interval.").
				Default("5m").Duration()
		autoMaxInterval = kingpin.Flag("refresh.auto-max-interval", "Upper bound of the auto refresh interval.").
				Default("24h").Duration()

		cacheFile = kingpin.Flag("cache.file", "File to persist the cache to, so restarts do not refetch everything. Empty disables persistence.").
				Default("").String()
		cacheSaveInterval = kingpin.Flag("cache.save-interval", "How often to write the cache file if it changed.").
					Default("1m").Duration()

		extraLabels = kingpin.Flag("label", "Extra label added to every metric, e.g. --label=account=prod. Repeatable.").
				PlaceHolder("NAME=VALUE").StringMap()
		hostLabels = kingpin.Flag("collector.host-labels", "Comma-separated Servers.com host label keys to expose in serverscom_host_labels; '*' for all.").
				Default("*").String()
		metricsPath = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").
				Default("/metrics").String()
		toolkitFlags = webflag.AddFlags(kingpin.CommandLine, ":9920")
	)

	// --[no-]collector.<name> for every optional collector.
	collectorFlags := map[cache.Collector]*bool{}
	for _, c := range cache.OptionalCollectors {
		collectorFlags[c] = kingpin.Flag("collector."+string(c), collectorHelp[c]+" Disable with --no-collector."+string(c)+".").
			Default("true").Bool()
	}

	// --refresh.<name>-interval for every collector: a duration or auto.
	intervals := map[cache.Collector]refresher.Interval{}
	intervalFlags := map[cache.Collector]*refresher.Interval{}
	for _, c := range cache.Collectors {
		intervalFlags[c] = &refresher.Interval{}
		kingpin.Flag("refresh."+string(c)+"-interval", "Refresh interval of "+string(c)+": a duration or auto. "+
			"Auto collectors share the budget left after the fixed ones.").Default("auto").SetValue(intervalFlags[c])
	}

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

	// Extra labels are added to every metric registered through reg.
	if err := validateExtraLabels(*extraLabels, collector.New(nil, nil, nil).LabelNames()); err != nil {
		logger.Error("Invalid extra label", "err", err)
		os.Exit(1)
	}
	registry := prometheus.NewRegistry()
	reg := prometheus.WrapRegistererWith(prometheus.Labels(*extraLabels), registry)
	reg.MustRegister(
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
		Registerer:    reg,
	})
	if err != nil {
		logger.Error("Cannot create API client", "err", err)
		os.Exit(1)
	}

	logger.Info("Configuration",
		"api_url", *apiURL, "requests_per_hour", *apiBudget,
		"ratelimit_reserve", *apiReserve, "cache_file", *cacheFile, "extra_labels", fmt.Sprint(*extraLabels))

	store := cache.NewStore(*cacheFile, logger.With("component", "cache"))
	store.Load()

	for c, i := range intervalFlags {
		intervals[c] = *i
	}
	if *apiPageSize < 1 || *apiPageSize > maxPageSize {
		logger.Error("Invalid --api.page-size", "value", *apiPageSize, "min", 1, "max", maxPageSize)
		os.Exit(1)
	}
	if *autoMinInterval <= 0 || *autoMaxInterval < *autoMinInterval {
		logger.Error("Invalid auto interval bounds", "min", *autoMinInterval, "max", *autoMaxInterval)
		os.Exit(1)
	}

	disabled := cache.DisabledCollectors{}
	for c, on := range collectorFlags {
		if !*on {
			disabled[c] = true
		}
	}

	reg.MustRegister(collector.New(store, strings.Split(*hostLabels, ","), disabled))

	ref := refresher.New(client, store, refresher.Config{
		HostsInterval:   *hostsInterval,
		Intervals:       intervals,
		AutoMinInterval: *autoMinInterval,
		AutoMaxInterval: *autoMaxInterval,
		Disabled:        disabled,
		PageSize:        *apiPageSize,
		BudgetPerHour:   *apiBudget,
		Reserve:         *apiReserve,
	}, logger.With("component", "refresher"), reg)

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

// collectorHelp describes the optional collectors for --help.
var collectorHelp = map[cache.Collector]string{
	cache.CollectorDrives:     "Fetch and expose drive slots (serverscom_host_drive_*). One request per server and refresh.",
	cache.CollectorPorts:      "Fetch and expose switch port connections (serverscom_host_connection_info, serverscom_host_connections). One request per server and refresh.",
	cache.CollectorNetworks:   "Fetch and expose IP networks of servers (serverscom_host_network_info). One request per server and refresh.",
	cache.CollectorFeatures:   "Fetch and expose server features (serverscom_host_feature_*). Also needed for serverscom_host_uplink_aggregated. One request per server and refresh.",
	cache.CollectorPowerFeeds: "Fetch and expose power feeds (serverscom_host_power_feed_*). One request per server and refresh.",
	cache.CollectorUplinks:    "Fetch uplink model catalogues and expose serverscom_host_uplink_*.",
	cache.CollectorL2Segments: "Fetch and expose L2 segments with members and networks (serverscom_l2_segment_*).",
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
