package ws

import (
	"context"
	"net/http"
)

// Listen starts a websocket-only server.
func Listen(ctx context.Context, addr string, handler http.Handler, certFile, keyFile string) error {
	mux := http.NewServeMux()
	mux.Handle("/ws", handler)
	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()
	if certFile != "" && keyFile != "" {
		return server.ListenAndServeTLS(certFile, keyFile)
	}
	return server.ListenAndServe()
}
