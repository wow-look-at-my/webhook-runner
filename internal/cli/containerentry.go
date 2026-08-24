package cli

import (
	"context"
	"net"
	"net/http"
	"time"
)

// containerEntryTimeout bounds the entry report end to end.
const containerEntryTimeout = 2 * time.Second

// reportContainerEntry tells the state API that this container is now
// executing, closing the span the host opened when it spawned `docker run`.
// See server.handleContainerEntry for why the far-side mark exists.
//
// Every failure is swallowed on purpose: no socket, no token, a server
// mid-restart, a manager instance that has no run to stamp. The hook must
// run exactly the same whether or not anyone is measuring it.
func reportContainerEntry(socketPath, token string) {
	if socketPath == "" || token == "" {
		return
	}
	client := &http.Client{
		Timeout: containerEntryTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	req, err := http.NewRequest(http.MethodPost, "http://state/phase/container-entry", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
