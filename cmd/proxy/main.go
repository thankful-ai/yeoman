package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thankful-ai/yeoman/internal/google"
	"github.com/thankful-ai/yeoman/internal/proxy"
	"github.com/thankful-ai/yeoman/internal/yeoman"
	"golang.org/x/crypto/acme/autocert"
)

const timeout = 60 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("c", "config.json", "config file")
	flag.Usage = func() {
		usage([]string{})
	}
	flag.Parse()

	conf, err := parseConfig(*configPath)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	var level slog.Level
	switch conf.Log.Level {
	case yeoman.LogLevelInfo, yeoman.LogLevelDefault:
		level = slog.LevelInfo
	case yeoman.LogLevelDebug:
		level = slog.LevelDebug
	default:
		return fmt.Errorf("invalid log level: %s", conf.Log.Level)
	}

	removeTime := func(groups []string, a slog.Attr) slog.Attr {
		// Remove time from the output.
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	var output slog.Handler
	switch conf.Log.Format {
	case yeoman.LogFormatJSON, yeoman.LogFormatDefault:
		output = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: removeTime,
		})
	case yeoman.LogFormatConsole:
		output = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: removeTime,
		})
	default:
		return fmt.Errorf("invalid log format: %s", conf.Log.Format)
	}
	log := slog.New(output)

	terra := proxy.CloudProviders{}
	for _, pr := range conf.ProviderRegistries {
		parts := strings.Split(pr.Provider, ":")
		if len(parts) != 4 {
			return fmt.Errorf("invalid cloud provider: %s",
				pr.Provider)
		}
		switch parts[0] {
		case "gcp":
			var (
				project = parts[1]
				region  = parts[2]
				zone    = parts[3]
			)
			regName, _, _ := strings.Cut(pr.Registry, "/")
			tfGCP, err := google.NewGCP(yeoman.HTTPClient(),
				project, region, zone, pr.ServiceAccount,
				regName, pr.Registry)
			if err != nil {
				return fmt.Errorf("gcp new: %w", err)
			}
			terra.Providers = append(terra.Providers, tfGCP)
		default:
			return fmt.Errorf("unknown cloud provider: %s",
				pr.Provider)
		}
	}

	// We cannot set a short timeout on the server because websockets don't
	// handle it well. Instead, the backend services must set timeouts.

	pp, err := newProxy(log, terra, conf)
	if err != nil {
		return fmt.Errorf("new proxy: %w", err)
	}

	apiHandler, err := pp.reverseProxy.RedirectHTTPHandler()
	if err != nil {
		return fmt.Errorf("redirect http handler: %w", err)
	}
	go func() {
		err = http.ListenAndServe(
			fmt.Sprintf(":%d", conf.HTTP.Port),
			pp.autocertManager.HTTPHandler(apiHandler))
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"listen and serve http: %s\n", err)
			os.Exit(1)
		}
	}()
	pp.server.Addr = fmt.Sprintf(":%d", conf.HTTPS.Port)
	go func() {
		if err = pp.server.ListenAndServeTLS("", ""); err != nil {
			fmt.Fprintf(os.Stderr,
				"listen and serve tls: %s\n", err)
			os.Exit(1)
		}
	}()

	log.Info("listening",
		slog.Int("httpPort", conf.HTTP.Port),
		slog.Int("httpsPort", conf.HTTPS.Port))

	sighupCh := make(chan bool)
	go checkHealth(log, pp.reverseProxy, sighupCh)
	gracefulRestart(log, pp.server, timeout)
	return nil
}

type proxyParts struct {
	reverseProxy    *proxy.ReverseProxy
	server          *http.Server
	autocertManager *autocert.Manager
}

func parseConfig(configPath string) (proxy.Config, error) {
	var conf proxy.Config
	byt, err := os.ReadFile(configPath)
	if err != nil {
		return conf, fmt.Errorf("read file: %w", err)
	}
	if err = json.Unmarshal(byt, &conf); err != nil {
		return conf, fmt.Errorf("unmarshal: %w", err)
	}
	return conf, nil
}

// checkHealth of backend servers constantly. We cancel the current health
// check when the reloaded channel receives a message, so a new health check
// with the new registry can be started.
func checkHealth(
	log *slog.Logger,
	rp *proxy.ReverseProxy,
	sighupCh <-chan bool,
) {
	if err := rp.CheckHealth(); err != nil {
		log.Error("failed to check health",
			slog.String("error", err.Error()))
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			err := rp.CheckHealth()
			if err != nil {
				log.Error("failed to check health",
					slog.String("error", err.Error()))
			}
		case <-sighupCh:
			return
		}
	}
}

// gracefulRestart listens for an interupt or terminate signal. When either is
// received, it stops accepting new connections and allows all existing
// connections time to complete. If connections do not shut down in time, this
// exits with 1.
func gracefulRestart(
	log *slog.Logger,
	srv *http.Server,
	timeout time.Duration,
) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("failed to shutdown server gracefully",
			slog.String("error", err.Error()))
		os.Exit(1)
	}
	log.Info("shut down")
}

func newProxy(
	log *slog.Logger,
	terra proxy.CloudProviders,
	conf proxy.Config,
) (*proxyParts, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c, err := google.NewCache(ctx, conf.HTTPS.CertBucket)
	if err != nil {
		return nil, fmt.Errorf("new cache: %w", err)
	}
	m := &autocert.Manager{
		Cache:      c,
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(conf.HTTPS.Host),
	}
	rp, err := proxy.New(ctx, log, terra, conf, m, timeout)
	if err != nil {
		return nil, fmt.Errorf("proxy new: %w", err)
	}
	getCert := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := m.GetCertificate(hello)
		if err != nil {
			return nil, fmt.Errorf("failed to get cert: %w", err)
		}
		return cert, nil
	}
	srv := &http.Server{
		Handler:        rp,
		ReadTimeout:    timeout,
		WriteTimeout:   timeout,
		MaxHeaderBytes: 1 << 20,
		TLSConfig: &tls.Config{
			GetCertificate: getCert,
			MinVersion:     tls.VersionTLS12,
		},
	}
	return &proxyParts{
		reverseProxy:    rp,
		server:          srv,
		autocertManager: m,
	}, nil
}

func usage(issues []string) {
	fmt.Println(`usage:

	proxy [options...]

global options:

	[-c]    path to config.json`)

	if len(issues) > 0 {
		fmt.Printf("errors:\n\n")
		for _, issue := range issues {
			fmt.Println("\t" + issue)
		}
	}
}
