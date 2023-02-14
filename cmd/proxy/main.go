package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/egtann/thankful-ai/terrafirma/gcp"
	"github.com/rs/zerolog"
	"github.com/thankful-ai/yeoman"
	"github.com/thankful-ai/yeoman/proxy"
	tf "github.com/thankful-ai/yeoman/terrafirma"
)

const timeout = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	output := zerolog.ConsoleWriter{Out: os.Stdout}
	log := zerolog.New(output).With().Timestamp().Logger()

	portTmp := flag.String("p", "3000", "port")
	configPath := flag.String("c", "config.json", "config file")
	// sslURL := flag.String("url", "", "enable ssl on the proxy's url (optional)")
	flag.Usage = func() {
		usage([]string{})
	}
	flag.Parse()

	issues := []string{}
	port := strings.TrimLeft(*portTmp, ":")
	portInt, err := strconv.Atoi(port)
	if err != nil {
		issues = append(issues, "port must be an integer")
	}
	if portInt < 0 {
		issues = append(issues, "port cannot be negative")
	}

	/*
		var selfURL *url.URL
		if len(*sslURL) > 0 {
			selfURL, err = url.ParseRequestURI(*sslURL)
			if err != nil {
				issues = append(issues, "invalid url")
			}
		}
	*/

	// TODO(egtann) via a config file?
	providerRegistries := map[tf.CloudProviderName]yeoman.ContainerRegistry{
		"gcp:personal-199119:us-central1:us-central1-b": yeoman.ContainerRegistry{
			Name: "us-central1-docker.pkg.dev",
			Path: "us-central1-docker.pkg.dev/personal-199119/yeoman-dev",
		},
	}
	terra := tf.New(5 * time.Minute)
	for cp, registry := range providerRegistries {
		parts := strings.Split(string(cp), ":")
		if len(parts) != 4 {
			return fmt.Errorf("invalid cloud provider: %s", cp)
		}
		providerLog := log.With().Str("provider", "gcp").Logger()
		switch parts[0] {
		case "gcp":
			var (
				project = parts[1]
				region  = parts[2]
				zone    = parts[3]
			)
			tfGCP, err := gcp.New(providerLog, proxy.HTTPClient(),
				project, region, zone, registry.Name,
				registry.Path)
			if err != nil {
				return fmt.Errorf("gcp new: %w", err)
			}
			terra.WithProvider(cp, tfGCP)
		default:
			return fmt.Errorf("unknown cloud provider: %s", cp)
		}
	}
	if len(issues) > 0 {
		usage(issues)
		os.Exit(1)
	}
	config, err := parseConfig(*configPath)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	rp, err := proxy.NewProxy(log, terra, config, timeout)
	if err != nil {
		return fmt.Errorf("new proxy: %w", err)
	}

	// Set up the API only if Subnet is configured and the internal
	// IP of the proxy server can be determined.
	srv := &http.Server{
		Handler:        rp,
		ReadTimeout:    timeout,
		WriteTimeout:   timeout,
		MaxHeaderBytes: 1 << 20,
	}

	// TODO(egtann) add support for SSL, not just on the hosts at boot, but
	// any hosts discovered while running.
	/*
		if len(*sslURL) > 0 {
			hosts := append(reg.Hosts(), selfURL.Host)
			m := &autocert.Manager{
				Cache:      autocert.DirCache("certs"),
				Prompt:     autocert.AcceptTOS,
				HostPolicy: autocert.HostWhitelist(hosts...),
			}
			getCert := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				log.Printf("get cert for %s", hello.ServerName)
				cert, err := m.GetCertificate(hello)
				if err != nil {
					log.Println("failed to get cert:", err)
				}
				return cert, err
			}
			srv.TLSConfig = &tls.Config{GetCertificate: getCert}
			apiHandler, err := rp.RedirectHTTPHandler()
			if err != nil {
				return fmt.Errorf("redirect http handler: %w", err)
			}
			go func() {
				err = http.ListenAndServe(":80", m.HTTPHandler(apiHandler))
				if err != nil {
					log.Fatal(fmt.Printf("listen and serve: %s", err))
				}
			}()
			port = "443"
			srv.Addr = ":443"
			go func() {
				log.Println("serving tls")
				if err = srv.ListenAndServeTLS("", ""); err != nil {
					log.Fatal(err)
				}
			}()
		} else {
		}
	*/

	srv.Addr = ":" + port
	go func() {
		// Send 2 because we're listening for two
		if err = srv.ListenAndServe(); err != nil {
			log.Fatal().Err(err).Msg("listen and serve")
		}
	}()

	log.Info().Str("port", port).Msg("listening")
	if err = rp.CheckHealth(); err != nil {
		log.Error().Err(err).Msg("check health")
	}
	sighupCh := make(chan bool)
	go checkHealth(rp, sighupCh)
	gracefulRestart(log, srv, timeout)

	return nil
}

// checkHealth of backend servers constantly. We cancel the current health
// check when the reloaded channel receives a message, so a new health check
// with the new registry can be started.
func checkHealth(proxy *proxy.ReverseProxy, sighupCh <-chan bool) {
	for {
		select {
		case <-time.After(3 * time.Second):
			err := proxy.CheckHealth()
			if err != nil {
				log.Println("check health", err)
			}
		case <-sighupCh:
			return
		}
	}
}

// gracefulRestart listens for an interupt or terminate signal. When either is
// received, it stops accepting new connections and allows all existing
// connections up to 10 seconds to complete. If connections do not shut down in
// time, this exits with 1.
func gracefulRestart(
	log zerolog.Logger,
	srv *http.Server,
	timeout time.Duration,
) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info().Msg("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("failed to shutdown server gracefully")
		os.Exit(1)
	}
	log.Info().Msg("shut down")
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

func usage(issues []string) {
	fmt.Print(`usage:

    proxy [options...]

global options:

    [-p]    port, default "3000"
    [-c]    config file, default "config.json"
    [-url]  url of the reverse proxy for https

config file:

    The config file contains JSON that maps your frontend hosts to backends. It
    needs to be defined. For example:

    {
        "127.0.0.1:3000": {
	    "HealthPath": "/health",
	    "Backends": [
                "127.0.0.1:3001",
                "127.0.0.1:3002"
	    ]
	}
    }

    Available options for each frontend are: HealthPath, Backends.

    If HealthPath is provided, proxy will check the health of the backend
    servers every few seconds and remove any from rotation until they come back
    online.

notes:

    * The url flag is optional. If provided, proxy will use https. If not
      provided (such as when testing on 127.0.0.1), proxy will use http.

    * After terminating TLS, proxy communicates over HTTP (plaintext) to the
      backend servers. Some cloud providers automatically encrypt traffic over
      their internal IP network (including Google Cloud). Check to ensure that
      your cloud provider does this before using proxy in production.

`)
	if len(issues) > 0 {
		fmt.Printf("errors:\n\n")
		for _, issue := range issues {
			fmt.Println("    " + issue)
		}
	}
}
