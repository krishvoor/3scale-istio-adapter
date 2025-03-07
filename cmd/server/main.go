package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/3scale/3scale-authorizer/pkg/authorizer"
	"github.com/3scale/3scale-authorizer/pkg/backend/v1"
	"github.com/3scale/3scale-istio-adapter/cmd/server/internal/metrics"
	"github.com/3scale/3scale-istio-adapter/pkg/threescale"
	"github.com/spf13/viper"

	"google.golang.org/grpc/grpclog"

	"istio.io/istio/pkg/log"
)

var version string

const (
	defaultListenAddr = "3333"

	defaultSystemCacheRetries                = 1
	defaultSystemCacheTTLSeconds             = 300
	defaultSystemCacheRefreshIntervalSeconds = 180
	defaultSystemCacheSize                   = 1000

	defaultMetricsEndpoint = "/metrics"
	defaultMetricsPort     = 8080

	defaultBackendCacheFlushInterval = time.Second * 15
)

func init() {
	viper.BindEnv("log_level")
	viper.BindEnv("log_json")
	viper.BindEnv("log_grpc")
	viper.BindEnv("listen_addr")
	viper.BindEnv("report_metrics")
	viper.BindEnv("metrics_port")

	viper.BindEnv("cache_ttl_seconds")
	viper.BindEnv("cache_refresh_seconds")
	viper.BindEnv("cache_entries_max")

	viper.BindEnv("client_timeout_seconds")
	viper.BindEnv("allow_insecure_conn")
	viper.BindEnv("root_ca")
	viper.BindEnv("client_cert")
	viper.BindEnv("client_key")

	viper.BindEnv("grpc_conn_max_seconds")

	viper.BindEnv("use_cached_backend")
	viper.BindEnv("backend_cache_flush_interval_seconds")
	viper.BindEnv("backend_cache_policy_fail_closed")

	configureLogging()
}

func configureLogging() {
	options := log.DefaultOptions()
	loglevel := viper.GetString("log_level")
	parsedLogLevel := stringToLogLevel(loglevel)
	options.SetOutputLevel(log.DefaultScopeName, parsedLogLevel)
	options.JSONEncoding = viper.GetBool("log_json")

	if !viper.GetBool("log_grpc") {
		options.LogGrpc = false
		grpclog.SetLoggerV2(
			grpclog.NewLoggerV2WithVerbosity(ioutil.Discard, ioutil.Discard, ioutil.Discard, 0),
		)
	}

	log.Configure(options)
}

func stringToLogLevel(loglevel string) log.Level {

	stringToLevel := map[string]log.Level{
		"debug": log.DebugLevel,
		"info":  log.InfoLevel,
		"warn":  log.WarnLevel,
		"error": log.ErrorLevel,
		"none":  log.NoneLevel,
	}

	if val, ok := stringToLevel[strings.ToLower(loglevel)]; ok {
		return val
	}
	return log.InfoLevel
}

func parseMetricsConfig() *authorizer.MetricsReporter {
	if !viper.IsSet("report_metrics") || !viper.GetBool("report_metrics") {
		return nil
	}

	port := defaultMetricsPort
	if viper.IsSet("metrics_port") {
		port = viper.GetInt("metrics_port")
	}

	metrics.Register()
	http.Handle(defaultMetricsEndpoint, metrics.GetHandler())
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to start metrics server %v", err)
	}
	go http.Serve(listener, nil)
	log.Infof("Serving metrics on port %d", port)

	return &authorizer.MetricsReporter{
		ReportMetrics: true,
		ResponseCB:    metrics.ReportCB,
		CacheHitCB:    metrics.IncrementCacheHits,
	}
}

func parseClientConfig() *http.Client {
	c := &http.Client{
		// Setting some sensible default here for http timeouts
		Timeout: time.Duration(time.Second * 10),
	}

	if viper.IsSet("client_timeout_seconds") {
		c.Timeout = time.Duration(viper.GetInt("client_timeout_seconds")) * time.Second
	}

	tlsConfig := tls.Config{}
	useTlsConfig := false

	if viper.IsSet("allow_insecure_conn") {
		tlsConfig.InsecureSkipVerify = viper.GetBool("allow_insecure_conn")
		useTlsConfig = true
	}

	if viper.IsSet("root_ca") {
		rootCAPath := viper.GetString("root_ca")
		if rootCAPath != "" {
			var pool, err = x509.SystemCertPool()
			if err != nil {
				log.Errorf("failed to read system certificates %v, trying to read CA certs anyway", err)
				pool = x509.NewCertPool()
			}

			pemCerts, err := ioutil.ReadFile(rootCAPath)
			if err != nil {
				log.Fatalf("failed to read root CA file %s - %v", rootCAPath, err)
			} else {
				if ok := pool.AppendCertsFromPEM(pemCerts); ok {
					tlsConfig.RootCAs = pool
					useTlsConfig = true
				} else {
					log.Fatalf("failed to parse root CA certificates %v", err)
				}
			}
		}
	}

	if viper.IsSet("client_cert") {
		clientCertFile := viper.GetString("client_cert")
		if clientCertFile != "" && viper.IsSet("client_key") {
			clientKeyFile := viper.GetString("client_key")
			if clientKeyFile != "" {
				var cert, err = tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
				if err != nil {
					log.Fatalf("error creating X509 key pair from %s and %s - %v", clientCertFile, clientKeyFile, err)
				} else {
					tlsConfig.Certificates = []tls.Certificate{ cert }
					useTlsConfig = true
				}
			} else {
				log.Fatalf("empty client_key path")
			}
		} else {
			log.Fatalf("both client_cert and client_key must be provided if you set any of them")
		}
	}

	if useTlsConfig {
		transport := &http.Transport{
			TLSClientConfig: &tlsConfig,
		}
		c.Transport = transport
	}

	return c
}

func createSystemCache() *authorizer.SystemCache {
	cacheTTL := defaultSystemCacheTTLSeconds
	cacheEntriesMax := defaultSystemCacheSize
	cacheUpdateRetries := defaultSystemCacheRetries
	cacheRefreshInterval := defaultSystemCacheRefreshIntervalSeconds

	if viper.IsSet("cache_ttl_seconds") {
		cacheTTL = viper.GetInt("cache_ttl_seconds")
	}

	if viper.IsSet("cache_refresh_seconds") {
		cacheRefreshInterval = viper.GetInt("cache_refresh_seconds")
	}

	if viper.IsSet("cache_entries_max") {
		cacheEntriesMax = viper.GetInt("cache_entries_max")
	}

	if viper.IsSet("cache_refresh_retries") {
		cacheUpdateRetries = viper.GetInt("cache_refresh_retries")
	}

	config := authorizer.SystemCacheConfig{
		MaxSize:               cacheEntriesMax,
		NumRetryFailedRefresh: cacheUpdateRetries,
		RefreshInterval:       time.Duration(cacheRefreshInterval) * time.Second,
		TTL:                   time.Duration(cacheTTL) * time.Second,
	}

	return authorizer.NewSystemCache(config, make(chan struct{}))
}

func createBackendConfig() authorizer.BackendConfig {
	logger := log.FindScope(log.DefaultScopeName)

	if viper.GetBool("use_cached_backend") {
		interval := time.Second * time.Duration(viper.GetInt("backend_cache_flush_interval_seconds"))
		if interval == 0 {
			interval = defaultBackendCacheFlushInterval
		}

		log.Infof("backend cache set to flush at %s intervals", interval.String())

		return authorizer.BackendConfig{
			EnableCaching:      true,
			CacheFlushInterval: interval,
			Logger:             logger,
			Policy:             getFailurePolicy(),
		}
	}

	return authorizer.BackendConfig{
		Logger: logger,
	}
}

func getFailurePolicy() backend.FailurePolicy {
	policy := backend.FailClosedPolicy

	if viper.IsSet("backend_cache_policy_fail_closed") && !viper.GetBool("backend_cache_policy_fail_closed") {
		policy = backend.FailOpenPolicy
		log.Infof("backend cache fail policy set to open")
	} else {
		log.Infof("backend cache fail policy set to closed")
	}

	return policy
}

func main() {
	var addr string

	if viper.IsSet("listen_addr") {
		addr = viper.GetString("listen_addr")
	} else {
		addr = defaultListenAddr
	}

	grpcKeepAliveFor := time.Minute
	if viper.IsSet("grpc_conn_max_seconds") {
		grpcKeepAliveFor = time.Second * time.Duration(viper.GetInt("grpc_conn_max_seconds"))
	}

	authorizer := authorizer.NewManager(
		parseClientConfig(),
		createSystemCache(),
		createBackendConfig(),
		parseMetricsConfig(),
	)

	adapterConf := &threescale.AdapterConfig{
		Authorizer:      authorizer,
		KeepAliveMaxAge: grpcKeepAliveFor,
	}

	s, err := threescale.NewThreescale(addr, adapterConf)
	if err != nil {
		log.Fatalf("Unable to start server: %v", err)
	}

	shutdown := make(chan error, 1)
	go func() {
		if version == "" {
			version = "undefined"
		}
		log.Infof("Starting server version %s", version)
		s.Run(shutdown)
	}()

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case sig := <-sigC:
			log.Infof("\n%s received. Attempting graceful shutdown\n", sig.String())
			authorizer.Shutdown()
			err := s.Close()
			if err != nil {
				log.Fatalf("Error calling graceful shutdown")
			}

		case err = <-shutdown:
			if err != nil {
				log.Fatalf("gRPC server has shut down: err %v", err)
			}

			log.Info("gRPC server has shut down gracefully")
			return
		}
	}
}
