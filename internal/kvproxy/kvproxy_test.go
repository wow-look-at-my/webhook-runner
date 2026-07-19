package kvproxy

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestServe verifies the TCP->Unix proxy: bytes written to a TCP client come
// back from an echo server listening on the Unix socket.
func TestServe(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	uln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer uln.Close()
	go func() {
		for {
			c, err := uln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }() // echo
		}
	}()

	ln, err := Serve("127.0.0.1:0", sock)
	require.NoError(t, err)
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

// TestServeBadAddr returns an error rather than panicking on a bad address.
func TestServeBadAddr(t *testing.T) {
	_, err := Serve("not-an-address", "/tmp/x.sock")
	require.Error(t, err)
}

// A dial during the server-handover window (socket briefly absent) must
// wait it out, not fail the hook's request — the launch-during-restart fix.
func TestForwardSurvivesLateSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "state.sock")

	ln, err := Serve("127.0.0.1:0", sock)
	require.NoError(t, err)
	defer ln.Close()

	// Upstream appears 700ms AFTER the client connects (mid-handover).
	go func() {
		time.Sleep(700 * time.Millisecond)
		up, err := net.Listen("unix", sock)
		if err != nil {
			return
		}
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err == nil {
			_, _ = c.Write([]byte("pong"))
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 4)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err, "the shim must retry the dial across the handover window")
	require.Equal(t, "pong", string(buf))
}

// After the socket is unlinked and re-bound (a new server), the NEXT
// connection reaches the new listener — old established streams are not
// silently replayed, new ones just work.
func TestForwardReachesReplacedSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "state.sock")
	serveOnce := func(reply string) net.Listener {
		up, err := net.Listen("unix", sock)
		require.NoError(t, err)
		go func() {
			for {
				c, err := up.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					buf := make([]byte, 4)
					if _, err := io.ReadFull(c, buf); err == nil {
						_, _ = c.Write([]byte(reply))
					}
				}(c)
			}
		}()
		return up
	}
	old := serveOnce("old!")
	ln, err := Serve("127.0.0.1:0", sock)
	require.NoError(t, err)
	defer ln.Close()

	roundtrip := func() string {
		c, err := net.Dial("tcp", ln.Addr().String())
		require.NoError(t, err)
		defer c.Close()
		_, _ = c.Write([]byte("ping"))
		require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
		buf := make([]byte, 4)
		_, err = io.ReadFull(c, buf)
		require.NoError(t, err)
		return string(buf)
	}
	require.Equal(t, "old!", roundtrip())

	// Handover: unlink + rebind (what a restarting server does).
	old.Close()
	_ = os.Remove(sock) // Close already unlinks; a restarting server also pre-removes
	fresh := serveOnce("new!")
	defer fresh.Close()
	require.Equal(t, "new!", roundtrip())
}
