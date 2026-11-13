package windows

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"

	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
)

type stalledFileConnectivity struct {
	connectivity
	stage      string
	reached    chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	sftpConn   net.Conn
	serverDone chan struct{}
}

func (conn *stalledFileConnectivity) stall() error {
	close(conn.reached)
	<-conn.closed
	return net.ErrClosed
}

func (conn *stalledFileConnectivity) run(string) (string, error) {
	if conn.stage == "command" {
		return "", conn.stall()
	}
	if conn.stage == "success" {
		return "True", nil
	}
	return "False", nil
}

func (conn *stalledFileConnectivity) createSFTPClient() (*sftp.Client, error) {
	if conn.stage == "sftp" {
		return nil, conn.stall()
	}
	return sftp.NewClientPipe(conn.sftpConn, conn.sftpConn)
}

func (conn *stalledFileConnectivity) transfer(*sftp.Client, io.Reader, string, string) error {
	return conn.stall()
}

func (conn *stalledFileConnectivity) close() error {
	conn.closeOnce.Do(func() {
		close(conn.closed)
		if conn.sftpConn != nil {
			_ = conn.sftpConn.Close()
		}
	})
	return nil
}

func TestEnsureFileWithContext(t *testing.T) {
	for _, stage := range []string{"command", "sftp", "transfer"} {
		for _, deadline := range []bool{false, true} {
			name := stage + "/cancel"
			if deadline {
				name = stage + "/deadline"
			}
			t.Run(name, func(t *testing.T) {
				conn := &stalledFileConnectivity{stage: stage, reached: make(chan struct{}), closed: make(chan struct{})}
				defer conn.close()
				if stage == "transfer" {
					clientSide, serverSide := net.Pipe()
					conn.sftpConn = clientSide
					server, err := sftp.NewServer(serverSide)
					require.NoError(t, err)
					conn.serverDone = make(chan struct{})
					go func() {
						defer close(conn.serverDone)
						_ = server.Serve()
					}()
					defer func() {
						_ = server.Close()
						<-conn.serverDone
					}()
				}
				filePath := filepath.Join(t.TempDir(), "webconfig.tar.gz")
				require.NoError(t, os.WriteFile(filePath, []byte("payload"), 0600))
				vm := &windows{interact: conn, log: logr.Discard()}
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
				}
				defer cancel()
				result := make(chan error, 1)
				go func() {
					result <- vm.EnsureFileWithContext(ctx, &payload.CompressedFileInfo{Path: filePath}, TLSDir)
				}()
				select {
				case <-conn.reached:
				case <-time.After(5 * time.Second):
					t.Fatal("file transfer did not reach expected stage")
				}
				if !deadline {
					cancel()
				}
				select {
				case err := <-result:
					if deadline {
						require.ErrorIs(t, err, context.DeadlineExceeded)
					} else {
						require.ErrorIs(t, err, context.Canceled)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("file transfer did not stop")
				}
				require.Eventually(t, func() bool {
					select {
					case <-conn.closed:
						return true
					default:
						return false
					}
				}, time.Second, time.Millisecond)
			})
		}
	}
	t.Run("pre-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		vm := &windows{}
		require.ErrorIs(t, vm.EnsureFileWithContext(ctx, nil, TLSDir), context.Canceled)
	})
	t.Run("success preserves connection", func(t *testing.T) {
		conn := &stalledFileConnectivity{stage: "success", closed: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		vm := &windows{interact: conn}
		require.NoError(t, vm.EnsureFileWithContext(ctx, &payload.CompressedFileInfo{Path: "webconfig.tar.gz"}, TLSDir))
		cancel()
		select {
		case <-conn.closed:
			t.Fatal("successful operation left cancellation callback registered")
		default:
		}
	})
}
