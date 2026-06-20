package cli

import (
	"errors"
	"io"
	"net"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// kvForwardListen is where the shim listens inside the hook container; hooks
// reach the KV API at HOOK_KV_URL=http://localhost:9002.
const kvForwardListen = "127.0.0.1:9002"

// kvForwardCmd is the in-container shim the runner sets as the entrypoint of a
// state hook. It proxies a localhost TCP port to the bind-mounted KV Unix
// socket, then runs the hook's real command — so the hook reaches the KV API
// at a plain http://localhost:9002 URL with any HTTP client, no --unix-socket
// and no networking. webhook-runner injects its own binary as the shim.
var kvForwardCmd = &cobra.Command{
	Use:                "kv-forward [command...]",
	Short:              "Internal: proxy localhost:9002 to the KV Unix socket, then exec the hook command",
	Hidden:             true,
	DisableFlagParsing: true, // all args belong to the wrapped command, not us
	RunE:               runKVForward,
}

func init() { rootCmd.AddCommand(kvForwardCmd) }

func runKVForward(_ *cobra.Command, args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("kv-forward: no command to run")
	}
	socket := os.Getenv("HOOK_KV_SOCKET")
	if socket == "" {
		return errors.New("kv-forward: HOOK_KV_SOCKET not set")
	}
	listen := firstNonEmpty(os.Getenv("HOOK_KV_LISTEN"), kvForwardListen)

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go proxyToSocket(c, socket)
		}
	}()

	// Run the hook's real command, inheriting stdio so the runner captures its
	// output, and exit with its status. The proxy lives only as long as it.
	child := exec.Command(args[0], args[1:]...) //nolint:gosec // argv is the hook's own command
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = child.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	return err
}

// proxyToSocket copies bytes both ways between a TCP client and the KV Unix
// socket until either side closes.
func proxyToSocket(client net.Conn, socket string) {
	defer client.Close()
	up, err := net.Dial("unix", socket)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, up); done <- struct{}{} }()
	<-done
}
