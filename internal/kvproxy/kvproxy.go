// Package kvproxy is the transport shim that lets state hooks reach the KV API
// at a plain TCP URL. webhook-runner injects its own binary as a state hook's
// container entrypoint; that binary runs Serve to proxy a localhost TCP port to
// the bind-mounted KV Unix socket, then execs the hook's real command — so the
// hook uses http://localhost:9002 with any HTTP client, no --unix-socket and no
// networking.
package kvproxy

import (
	"io"
	"net"
	"time"
)

// Connect-phase retry: a runner restart replaces the state socket (the new process unlinks and re-binds $TMPDIR/whr-state.sock), so for a.
const (
	dialRetryEvery = 250 * time.Millisecond
	dialRetryFor   = 10 * time.Second
)

// Serve starts a TCP listener on listenAddr that proxies every accepted
// connection to the Unix socket at socketPath. It returns the listener; close
// it to stop accepting (in-flight connections drain on their own).
func Serve(listenAddr, socketPath string) (net.Listener, error) {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go forward(c, socketPath)
		}
	}()
	return ln, nil
}

// forward copies bytes both ways between a TCP client and the KV Unix socket
// until either side closes. The upstream dial survives a server handover
// (see the retry constants above).
func forward(client net.Conn, socketPath string) {
	defer client.Close()
	up, err := dialWithRetry(socketPath)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
}

// dialWithRetry dials the KV socket, retrying connect-phase failures on a
// flat cadence for the handover window. Deliberately no backoff and no
// jitter: the window is short, local, and bounded.
func dialWithRetry(socketPath string) (net.Conn, error) {
	deadline := time.Now().Add(dialRetryFor)
	for {
		c, err := net.Dial("unix", socketPath)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(dialRetryEvery)
	}
}
