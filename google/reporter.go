package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/egtann/yeoman"
)

var _ yeoman.Reporter = &Reporter{}

type Reporter struct {
	service string
	version string
	url     string
}

type ReporterOpts struct {
	Service string
	Version string
	Project string
}

func NewReporter(opts ReporterOpts) error {
	return &Reporter{
		service: opts.Service,
		version: opts.Version,
		url: fmt.Sprintf(
			"https://clouderrorreporting.googleapis.com/v1beta1/projects/%s/events:report",
			opts.Project,
		),
	}
}

func (r *Reporter) Report(origErr error) {
	client := newClient()

	type serviceContext struct {
		Service string `json:"service"`
		Version string `json:"version"`
	}
	data := struct {
		ServiceContext serviceContext `json:"serviceContext"`
		Message        string         `json:"message"`
	}{
		serviceContext: serviceContext{
			Service: r.service,
			Version: r.version,
		},
		message: origErr.Error(),
	}
	logErr := func(origErr, reportErr error) {
		fmt.Fprintf(os.Stderr, "failed to report error (%v): %v\n",
			origErr, reportErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url,
		json.NewEncoder(data))
	if err != nil {
		logErr(origErr, fmt.Errorf("new request: %w", err))
		return
	}

	rsp, err := client.Do(req)
	if err != nil {
		logErr(origErr, fmt.Errorf("do: %w", err))
		return
	}
	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode != http.StatusCreated {
		logErr(origErr, fmt.Errorf("unexpected status code: %d",
			rsp.StatusCode))
		byt, _ := io.ReadAll(rsp.Body)
		fmt.Println(string(byt))
		return
	}
}

// newClient returns an HTTP client that doesn't share a global transport. The
// implementation is taken from github.com/hashicorp/go-cleanhttp.
func newClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   -1,
		DisableKeepAlives:     true,
	},
	}
}
