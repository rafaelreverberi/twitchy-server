// Test image only. Never built into the release binary or default image.
package main

import (
	"github.com/rafaelreverberi/twitchy-server/internal/server"
	"net/http"
	"os"
)

func main() {
	c, e := server.LoadConfig()
	if e != nil {
		os.Exit(1)
	}
	s, e := server.New(c, server.FixtureProvider{File: "/app/fixture.ts"})
	if e != nil {
		os.Exit(1)
	}
	defer s.Close()
	_ = http.ListenAndServe(c.Listen, s)
}
