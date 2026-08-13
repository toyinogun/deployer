// Command envprint is the app that proves the configuration path from spec 0010
// reaches a running container. It logs every environment variable it was given,
// so PORT, APP_URL, and each configured key are visible in get_logs.
//
// It also logs one ordinary sentence carrying the value of the key named in
// SENTENCE_KEY, which is how the redaction rule is checked both ways: a secret
// value eight characters or longer is blanked out of that sentence, and a
// shorter one is left alone with the sentence intact.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	env := os.Environ()
	sort.Strings(env)
	for _, entry := range env {
		log.Printf("env %s", entry)
	}

	if key := strings.TrimSpace(os.Getenv("SENTENCE_KEY")); key != "" {
		log.Printf("the value of %s today is %s and the app carried on as usual", key, os.Getenv(key))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "envprint on host %s\n", r.Host)
	})
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("envprint listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}
