package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/saim61/podium/internal/config"
	"github.com/stretchr/testify/require"
)

func serveConfig(addr string) config.HTTP {
	return config.HTTP{
		Addr:            addr,
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
		IdleTimeout:     5 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}
}

func TestServeStartsAndShutsDownGracefully(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 1)
	done := make(chan error, 1)

	go func() {
		done <- Serve(ctx, serveConfig("127.0.0.1:0"), discardLogger(), testRouter(t),
			func(addr string) { addrs <- addr })
	}()

	resp, err := http.Get("http://" + <-addrs + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
}

func TestServeFailsOnUnusableAddress(t *testing.T) {
	err := Serve(context.Background(), serveConfig("256.256.256.256:99999"),
		discardLogger(), testRouter(t), nil)

	require.Error(t, err)
}

func TestServeFailsWhenAddressAlreadyInUse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addrs := make(chan string, 1)
	go func() {
		_ = Serve(ctx, serveConfig("127.0.0.1:0"), discardLogger(), testRouter(t),
			func(addr string) { addrs <- addr })
	}()

	err := Serve(context.Background(), serveConfig(<-addrs), discardLogger(), testRouter(t), nil)
	require.Error(t, err)
}
