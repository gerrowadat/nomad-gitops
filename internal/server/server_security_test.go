package server

import (
	"net/http"
	"testing"

	"github.com/gerrowadat/nomad-botherer/internal/config"
)

func TestHTTPServer_HasTimeouts(t *testing.T) {
	s := &Server{
		cfg: &config.Config{ListenAddr: ":8080"},
		mux: http.NewServeMux(),
	}
	srv := s.newHTTPServer()
	if srv.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout not set; server is vulnerable to slowloris attacks")
	}
	if srv.ReadTimeout == 0 {
		t.Error("ReadTimeout not set")
	}
	if srv.WriteTimeout == 0 {
		t.Error("WriteTimeout not set")
	}
}
