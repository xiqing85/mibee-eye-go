package web

import (
	"os"
	"os/signal"
	"syscall"
)

// init registers a SIGTERM catch-all so that tests exercising handlePostConfigOnvif
// (which schedules a real SIGTERM to restart the server) don't kill the test process.
func init() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		for range sigCh {
			// Swallow SIGTERM — it is expected from config-save tests.
		}
	}()
}
