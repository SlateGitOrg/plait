package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/archit2501/plait/internal/fakecloud"
)

// fakecloud runs the in-repo fake cloud API until interrupted.
func (c *cli) fakecloud(ctx context.Context, args []string) error {
	fs, _ := c.flags("fakecloud", false)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	token := fs.String("token", "", "require this bearer token on every request")
	latency := fs.Int("latency-ms", 0, "artificial latency per API request, to make parallelism visible")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv := fakecloud.New(*token)
	srv.SetFaults(fakecloud.Faults{LatencyMS: *latency})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	c.printf("fakecloud listening on http://%s\n", ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
