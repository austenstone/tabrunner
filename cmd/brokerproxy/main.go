// Command brokerproxy is a tiny CORS reverse proxy for the GitHub Actions
// broker leg (broker.actions.githubusercontent.com), which is the only runner
// endpoint that does not send CORS headers. Everything else a runner talks to
// (api.github.com, vstoken/pipelines.actions.githubusercontent.com) already
// returns Access-Control-Allow-Origin: *, so a browser tab can reach them
// directly; the broker cannot.
//
// It uses a path-prefix scheme: the browser calls
//
//	https://proxy.example/https://broker.actions.githubusercontent.com/session?...
//
// The proxy strips its own origin, treats the remainder as an absolute target
// URL, forwards the request, and re-emits the response with permissive CORS
// headers. OPTIONS preflights are answered locally.
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":8732", "listen address")
	flag.Parse()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           http.HandlerFunc(handle),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("brokerproxy listening on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func handle(w http.ResponseWriter, r *http.Request) {
	setCORS(w)

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.URL.Path == "/" || r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "brokerproxy ok\n")
		return
	}

	target := extractTarget(r)
	if target == "" {
		http.Error(w, "brokerproxy: expected /https://host/path target", http.StatusBadRequest)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "brokerproxy: bad target: "+err.Error(), http.StatusBadGateway)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Origin")
	req.Header.Del("Referer")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "brokerproxy: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	setCORS(w) // re-assert in case upstream set conflicting values
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// extractTarget rebuilds the absolute upstream URL from the request path and
// raw query. The path is everything after the leading slash, e.g.
// "/https://broker.actions.githubusercontent.com/session" -> the URL itself.
func extractTarget(r *http.Request) string {
	target := strings.TrimPrefix(r.URL.RequestURI(), "/")
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return ""
	}
	return target
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if skipHeader(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func skipHeader(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Access-Control-Allow-Origin",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
		"Connection",
		"Content-Length":
		return true
	}
	return false
}

func setCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "*")
	h.Set("Access-Control-Expose-Headers", "*")
	h.Set("Access-Control-Max-Age", "86400")
}
