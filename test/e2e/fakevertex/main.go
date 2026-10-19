// Command fakevertex is a test-only stand-in for a regional Vertex AI endpoint.
// It serves HTTPS and echoes back which region handled the request, so the e2e
// test can prove that Envoy really connected to the rerouted host.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
)

type echo struct {
	ServedBy string            `json:"served_by"`
	Path     string            `json:"path"`
	Host     string            `json:"host"`
	Headers  map[string]string `json:"headers"`
}

func main() {
	region := flag.String("region", "", "region this instance pretends to be")
	addr := flag.String("addr", ":443", "listen address")
	cert := flag.String("cert", "/certs/server.pem", "TLS certificate")
	key := flag.String("key", "/certs/server-key.pem", "TLS key")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		e := echo{ServedBy: *region, Path: r.URL.RequestURI(), Host: r.Host, Headers: map[string]string{}}
		for k := range r.Header {
			e.Headers[http.CanonicalHeaderKey(k)] = r.Header.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(e)
	})
	log.Printf("fakevertex region=%s listening on %s", *region, *addr)
	log.Fatal(http.ListenAndServeTLS(*addr, *cert, *key, nil))
}
