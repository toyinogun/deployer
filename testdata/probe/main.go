// Command probe is a deployable app that reports what it can and cannot reach
// from inside an app namespace (spec 0008, AC-19).
//
// It is deployed through the real deploy_app path, twice under two slugs, so
// each instance is the other's sibling. Then GET /probe over its public hostname
// returns one row per destination.
//
// Every dial carries its own timeout, because a blocked destination under a
// NetworkPolicy is a silent drop rather than a refusal: an untimed dial to a
// fenced address hangs forever and the probe never answers.
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// dialTimeout is how long a blocked destination is allowed to say nothing before
// it is called blocked.
const dialTimeout = 3 * time.Second

// result is one attempted connection.
type result struct {
	Target  string `json:"target"`
	Address string `json:"address"`
	Outcome string `json:"outcome"` // reached, refused, timeout, dns_failed
	MS      int64  `json:"ms"`
}

// targets are the destinations with names the probe can work out for itself.
// The rest are addresses only kubectl knows, passed as query parameters:
//
//	/probe?sibling_pod=10.42.1.7:8080&sibling_service=10.43.2.9:80&node=172.16.70.11:6443&load_balancer=172.16.70.20:443
var targets = map[string]string{
	"kubernetes_api": "kubernetes.default.svc:443",
	"registry":       "deployer-registry.deployer-system.svc:5000",
	"control_plane":  "deployer.deployer-system.svc:80",
	"public_host":    "example.com:443",
}

// queryTargets are the ones with no name, only an address.
var queryTargets = []string{"sibling_pod", "sibling_service", "node", "load_balancer"}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	// Something to serve on the hostname, so the ingress path is exercised too.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("probe\n")); err != nil {
			log.Printf("writing the root response: %v", err)
		}
	})
	mux.HandleFunc("GET /probe", handleProbe)

	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("probe listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func handleProbe(w http.ResponseWriter, r *http.Request) {
	attempts := map[string]string{}
	for name, address := range targets {
		attempts[name] = address
	}
	// A named target can be overridden too, so a probe on a cluster with
	// different service names still reports something useful.
	for _, name := range append(queryTargets, keys(targets)...) {
		if v := strings.TrimSpace(r.URL.Query().Get(name)); v != "" {
			attempts[name] = v
		}
	}

	results := make([]result, 0, len(attempts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, address := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := dial(name, address)
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].Target < results[j].Target })

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Printf("writing the probe response: %v", err)
	}
}

// dial attempts one connection and classifies what came back. A fenced address
// under a deny policy times out; a reachable host with nothing listening
// refuses; a name the resolver will not answer for fails in DNS.
func dial(name, address string) result {
	started := time.Now()
	conn, err := net.DialTimeout("tcp", address, dialTimeout)
	elapsed := time.Since(started).Milliseconds()
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			log.Printf("closing the connection to %s: %v", address, closeErr)
		}
		return result{Target: name, Address: address, Outcome: "reached", MS: elapsed}
	}
	return result{Target: name, Address: address, Outcome: classify(err), MS: elapsed}
}

func classify(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_failed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "refused"
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
