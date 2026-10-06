package scale

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	// HopHeader marks a request the activator re-sent through the ingress
	// controller. If it comes back to the activator, the controller still
	// routes to the activator, and LoopHeader tells the sender to retry.
	HopHeader  = "X-Paas-Activator"
	LoopHeader = "X-Paas-Activator-Loop"

	// MaxBody bounds the request body held in memory while waking.
	MaxBody = 10 << 20
)

// Activator receives requests for sleeping deployments: the Service of a
// sleeping deployment points here (deploy.Kubernetes.Sleep). It wakes the
// deployment, holds the request until a pod is ready and proxies it.
type Activator struct {
	Scaler  *Scaler
	Cluster Cluster
	// Upstream selects where a woken request goes: "" or "pod" proxies to
	// the pod IP (the control plane runs in the cluster); a URL such as
	// http://127.0.0.1:80 re-sends it through the ingress controller (the
	// control plane runs outside the cluster and cannot reach pod IPs).
	Upstream string
	// Timeout bounds waking plus proxying setup. Zero means 2 minutes.
	Timeout time.Duration
	Log     *slog.Logger

	ingress *url.URL
	proxy   *httputil.ReverseProxy
}

type targetKey struct{}

// deadlineKey carries the time after which retryTransport stops retrying.
type deadlineKey struct{}

// Init validates Upstream and builds the proxy. Handler calls it.
func (a *Activator) Init() error {
	if a.Upstream != "" && a.Upstream != "pod" {
		u, err := url.Parse(a.Upstream)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("activator upstream %q: want \"pod\" or an http(s) URL", a.Upstream)
		}
		a.ingress = u
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	// The ingress hop stays inside the platform; its certificate is the
	// wildcard for the app hosts, not for the address dialled.
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	a.proxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			t := r.Context().Value(targetKey{}).(*url.URL)
			r.URL.Scheme, r.URL.Host = t.Scheme, t.Host
			if a.ingress != nil {
				r.Header.Set(HopHeader, "1")
			}
		},
		Transport:     &retryTransport{base: base},
		FlushInterval: -1, // stream responses (SSE) as they come
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			a.Log.Error("activator: proxy", "host", r.Host, "err", err)
			http.Error(w, "deployment woke up but did not answer", http.StatusBadGateway)
		},
	}
	return nil
}

func (a *Activator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(HopHeader) != "" {
		// Our own re-sent request came back: the route still points here.
		w.Header().Set(LoopHeader, "1")
		http.Error(w, "waking up", http.StatusServiceUnavailable)
		return
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	wl, err := backend(ctx, a.Cluster, host)
	if errors.Is(err, errNoRoute) {
		http.Error(w, "no deployment serves "+host, http.StatusNotFound)
		return
	}
	if err != nil {
		a.Log.Error("activator: route lookup", "host", host, "err", err)
		http.Error(w, "route lookup failed", http.StatusBadGateway)
		return
	}

	// Hold the body: the request may be sent more than once.
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		if len(body) > MaxBody {
			http.Error(w, "request body too large while the deployment wakes up", http.StatusRequestEntityTooLarge)
			return
		}
	}

	start := time.Now()
	if err := a.Scaler.Wake(ctx, wl); err != nil {
		a.Log.Error("activator: wake", "deployment", wl.Key(), "err", err)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "deployment is waking up, try again shortly", http.StatusServiceUnavailable)
		return
	}
	target := a.ingress
	if target == nil {
		addr, err := a.Cluster.PodAddr(ctx, wl.Namespace, wl.Name)
		if err != nil {
			a.Log.Error("activator: pod address", "deployment", wl.Key(), "err", err)
			http.Error(w, "deployment has no ready pod", http.StatusBadGateway)
			return
		}
		target = &url.URL{Scheme: "http", Host: addr}
	}
	if wl.Sleeping {
		a.Log.Info("activator: serving woken request", "host", host, "deployment", wl.Key(),
			"wait_ms", time.Since(start).Milliseconds())
	}

	// Faz 20: the timeout bounds waking and connecting only. The proxied
	// request lives as long as the client's: a WebSocket (Upgrade, which
	// httputil.ReverseProxy tunnels) or an SSE stream that wakes a
	// deployment must not be cut when the timeout passes.
	deadline, _ := ctx.Deadline()
	pctx := context.WithValue(context.WithValue(r.Context(), targetKey{}, target), deadlineKey{}, deadline)
	out := r.WithContext(pctx)
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	out.ContentLength = int64(len(body))
	a.proxy.ServeHTTP(w, out)
}

// retryTransport retries a request that looped back to the activator or
// could not connect: right after a wake the ingress controller or the pod
// may need a moment. It gives up when the request's context ends or the
// activator's timeout (deadlineKey) has passed.
type retryTransport struct {
	base http.RoundTripper
}

const retryPause = 100 * time.Millisecond

func (t *retryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	for {
		req := r
		if r.GetBody != nil {
			b, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			req = r.Clone(r.Context())
			req.Body = b
		}
		resp, err := t.base.RoundTrip(req)
		switch {
		case err == nil && resp.Header.Get(LoopHeader) == "":
			return resp, nil
		case err == nil:
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		case !retryable(err):
			return nil, err
		}
		if dl, ok := r.Context().Value(deadlineKey{}).(time.Time); ok && !dl.IsZero() && time.Now().After(dl) {
			if err == nil {
				err = errors.New("route still points at the activator")
			}
			return nil, fmt.Errorf("%w (%v)", context.DeadlineExceeded, err)
		}
		select {
		case <-r.Context().Done():
			if err == nil {
				err = errors.New("route still points at the activator")
			}
			return nil, fmt.Errorf("%w (%v)", r.Context().Err(), err)
		case <-time.After(retryPause):
		}
	}
}

func retryable(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial" ||
		strings.Contains(err.Error(), "connection refused")
}
