package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"golang.org/x/exp/slog"
)

// RedirectHTTPHandler redirects http requests to use the API if the request
// originated from the whitelisted subnet. In all other GET and HEAD requests,
// this handler redirects to HTTPS. For POST, PUT, etc. this handler throws an
// error letting the client know to use HTTPS.
func (rp *ReverseProxy) RedirectHTTPHandler() (http.Handler, error) {
	fn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET", "HEAD": // Do nothing
		default:
			http.Error(w, "Use HTTPS", http.StatusBadRequest)
			return
		}
		target := "https://" + stripPort(r.Host) + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusFound)
	})
	if len(rp.reg.Subnets) == 0 {
		return fn, nil
	}
	networks := make([]netip.Prefix, 0, len(rp.reg.Subnets))
	for _, prefix := range rp.reg.Subnets {
		pfx, err := netip.ParsePrefix(prefix)
		if err != nil {
			return nil, fmt.Errorf("parse prefix %s: %w", prefix,
				err)
		}
		networks = append(networks, pfx)
	}
	fn = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET", "HEAD": // Do nothing
		default:
			http.Error(w, "use https", http.StatusBadRequest)
			return
		}

		// If the host isn't an IP address, then we know we're not
		// communicating on a subnet and can redirect early.
		host := stripPort(r.Host)
		addr, err := netip.ParseAddr(host)
		if err != nil {
			target := "https://" + host + r.URL.RequestURI()
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		var isLAN bool
		for _, network := range networks {
			if network.Contains(addr) {
				isLAN = true
				break
			}
		}
		if isLAN {
			switch r.URL.Path {
			case "/health":
				_, _ = w.Write([]byte(`{"load":0}`))
				return
			case "/services":
				data := map[string][]netip.AddrPort{}
				reg := rp.cloneRegistry()
				for host, srv := range reg.Services {
					data[host] = srv.liveBackends
				}
				err = json.NewEncoder(w).Encode(data)
				if err != nil {
					rp.log.Error(
						"failed to encode registry",
						slog.String("error", err.Error()))
				}
				return
			}
		}
		target := "https://" + host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusFound)
	})
	return fn, nil
}

func stripPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}
