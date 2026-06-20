package kvproxy

import (
	"io"
	"net"
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
