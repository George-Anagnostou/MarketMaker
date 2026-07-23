// Command server runs the local durable market-making exchange and web UI.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

type server struct {
	indexPath string
	v2        *exchangeService
}

func newServer() *server {
	indexPath := os.Getenv("MMG_STATIC_INDEX")
	if indexPath == "" {
		indexPath = "web/static/index.html"
	}
	return &server{indexPath: indexPath, v2: newExchangeService("data/games")}
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, s.indexPath)
}

func main() {
	srv := newServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/api/v2/scenarios", srv.v2.handleScenarios)
	mux.HandleFunc("/api/v2/games", srv.v2.handleGames)
	mux.HandleFunc("/api/v2/games/", srv.v2.handleGame)

	server := &http.Server{
		Addr:              "127.0.0.1:8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("market-maker web listening on http://%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
