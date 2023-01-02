package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"

	"github.com/egtann/yeoman/proxy"
)

const baseURL = "https://api.cloudflare.com/client/v4/zones/%s/dns_records"

type Cloudflare struct {
	apiKey string
	client *http.Client
}

type Params struct {
	APIKey string
	ZoneID string
}

var _ proxy.DNSStore = &Cloudflare{}

func New(p Params) *Cloudflare {
	return &Cloudflare{
		apiKey:  p.APIKey,
		baseURL: fmt.Sprintf(baseURL, p.ZoneID),
		client:  cleanhttp.NewDefaultClient(),
	}
}

func (c *Cloudflare) List(ctx context.Context) ([]proxy.DNSEntry, error) {
	nextPage := true
	var entries []proxy.DNSEntry
	for page := 1; nextPage; page++ {
		var (
			tmpEntries []proxy.DNSEntry
			err        error
		)
		tmpEntries, nextPage, err = c.listPage(ctx, page)
		if err != nil {
			return nil, fmt.Errorf("list page %d: %w", err)
		}
		entries = append(entries, tmpEntries...)
	}
	return entries, nil
}

// list reports whether there's more pages to retrieve.
func (c *Cloudflare) listPage(
	ctx context.Context,
	page int,
) ([]proxy.DNSEntry, bool, error) {
	params := url.Values{
		"page":     []string{strconv.Itoa(page)},
		"per_page": []string{"100"},
	}
	uri := fmt.Sprintf("%s?%s", c.baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, uri, http.MethodGet, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	rsp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode != http.StatusOK {
		byt, _ := io.ReadAll(rsp.Body)
		return nil, fmt.Errorf("bad status code %d: %s",
			rsp.StatusCode, string(byt))
	}

	var data struct {
		Success    bool `json:"success"`
		ResultInfo struct {
			Count   int `json:"count"`
			Page    int `json:"page"`
			PerPage int `json:"per_page"`
		} `json:"result_info"`
		Result []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Content string `json:"content"`
			Type    string `json:"type"`
		} `json:"result"`
		Errors []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err = json.NewDecoder(rsp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if !data.Success {
		return nil, fmt.Errorf("failed: %v", data.Errors)
	}

	entries := make([]DNSEntry, 0, len(data.Result))
	for _, r := range data.Result {
		if r.Type != "A" {
			continue
		}
		addr, err := netip.ParseAddr(r.Content)
		if err != nil {
			// Ignore any DNS records containing non-IPs.
			continue
		}
		entries = append(entries, proxy.DNSEntry{
			ID:   r.ID,
			IP:   addr,
			Name: r.Name,
		})
	}
	ri := data.ResultInfo
	return entries, ri.Count > ri.Page*ri.PerPage, nil
}

func (c *Cloudflare) Create(ctx context.Context, entry proxy.DNSEntry) error {
	data := struct {
		Content  string   `json:"content"`
		Name     string   `json:"name"`
		Priority int      `json:"priority"`
		Tags     []string `json:"tags"`
		TTL      int      `json:"ttl"`
		Proxied  bool     `json:"proxied"`
		Comment  string   `json:"comment"`
	}{
		Content:  entry.IP.String(),
		Name:     entry.Name,
		Priority: 10,
		Tags:     []string{"yeoman"},
		TTL:      60,
	}
	byt, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL,
		bytes.NewReader(byt))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	rsp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode != http.StatusOK {
		byt, _ := io.ReadAll(rsp.Body)
		return nil, fmt.Errorf("bad status code %d: %s",
			rsp.StatusCode, string(byt))
	}
	return nil
}

func (c *Cloudflare) Delete(ctx context.Context, entry proxy.DNSEntry) error {
	uri := fmt.Sprintf("%s/%s", baseURL, entry.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, uri,
		nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	rsp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()

	if rsp.StatusCode != http.StatusOK {
		byt, _ := io.ReadAll(rsp.Body)
		return nil, fmt.Errorf("bad status code %d: %s",
			rsp.StatusCode, string(byt))
	}
	return nil
}

func (c *Cloudflare) do(
	ctx context.Context,
	req *http.Request,
) ([]byte, error) {
	req = req.WithContext(ctx)
	rsp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer func() { _ = rsp.Body.Close() }()

	byt, err := io.ReadAll(rsp.Body)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	return byt, nil
}
