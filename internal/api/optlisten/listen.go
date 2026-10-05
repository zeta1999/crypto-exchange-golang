// Package optlisten starts a small public options HTTP server and shuts it
// down when ctx is cancelled. Deribit, OKX, and Bybit share it.
package optlisten

import (
	"context"
	"net/http"
)

// Serve runs handler on addr until ctx is cancelled.
func Serve(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	return server.ListenAndServe()
}
