package google

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func NewReporter(opts ReporterOpts) *Reporter {
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
	client := yeoman.HTTPClient()

	type serviceContext struct {
		Service string `json:"service"`
		Version string `json:"version"`
	}
	data := struct {
		ServiceContext serviceContext `json:"serviceContext"`
		Message        string         `json:"message"`
	}{
		ServiceContext: serviceContext{
			Service: r.service,
			Version: r.version,
		},
		Message: origErr.Error(),
	}
	logErr := func(origErr, reportErr error) {
		fmt.Fprintf(os.Stderr, "failed to report error (%v): %v\n",
			origErr, reportErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(),
		10*time.Second)
	defer cancel()

	byt, err := json.Marshal(data)
	if err != nil {
		logErr(origErr, fmt.Errorf("marshal: %w", err))
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url,
		bytes.NewReader(byt))
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
