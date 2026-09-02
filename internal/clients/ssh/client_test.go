/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// slowSSHServer is a real, in-process SSH server that holds every "exec"
// session open for delay before replying, so tests can exercise Execute's
// context-cancellation path against the actual SSH wire protocol rather than
// a hand-written substitute for *ssh.Session.
type slowSSHServer struct {
	config *ssh.ServerConfig
	delay  time.Duration
}

func startSlowSSHServer(t *testing.T, delay time.Duration) (host string, port int) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("build host key signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil // accept any credential: auth is not what this harness tests
		},
	}
	config.AddHostKey(signer)

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	srv := &slowSSHServer{config: config, delay: delay}
	go srv.serve(listener)

	addr := listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (s *slowSSHServer) serve(listener net.Listener) {
	for {
		nConn, err := listener.Accept()
		if err != nil {
			return // listener closed: test is done
		}
		go s.handleConn(nConn)
	}
}

func (s *slowSSHServer) handleConn(nConn net.Conn) {
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, s.config)
	if err != nil {
		return
	}
	defer sConn.Close() //nolint:errcheck // best-effort cleanup in a test harness

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(channel, requests)
	}
}

// handleSession answers an "exec" request only after s.delay has elapsed,
// simulating a slow remote command (a multi-minute k3s install, scaled down
// for the test). It exits early -- without ever replying -- if the requests
// channel closes first, which is what happens when Execute aborts a session
// on context cancellation (closing the session tears down its request
// stream from the server's point of view).
func (s *slowSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		if req.WantReply {
			_ = req.Reply(true, nil)
		}

		select {
		case <-time.After(s.delay):
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, 0)
			_, _ = channel.Write([]byte("done"))
			_, _ = channel.SendRequest("exit-status", false, status)
			_ = channel.Close()
			return
		case _, ok := <-requests:
			if !ok {
				// Client tore down the session before the delay
				// elapsed -- exactly what Execute does on ctx
				// cancellation. Nothing left to reply to.
				return
			}
		}
	}
}

func newSlowTestClient(t *testing.T, host string, port int) *Client {
	t.Helper()

	c, err := NewClient(Config{
		Host:     host,
		Port:     port,
		Username: "test",
		Password: "test",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestExecuteReturnsPromptlyOnContextCancellation proves Execute does not
// block past the caller's deadline: a command that would otherwise take far
// longer than the context's timeout must still cause Execute to return at
// (approximately) the deadline, not at the command's real completion time.
func TestExecuteReturnsPromptlyOnContextCancellation(t *testing.T) {
	host, port := startSlowSSHServer(t, 5*time.Second)
	c := newSlowTestClient(t, host, port)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := c.Execute(ctx, "sleep 5")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when the context deadline is exceeded before the remote command finishes")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("want Execute to return at the context deadline (~200ms), got %s -- it blocked for the remote command's full duration instead", elapsed)
	}
}

// TestExecuteSucceedsWithinDeadline proves the context-awareness added to
// Execute does not break the ordinary, well-within-budget case.
func TestExecuteSucceedsWithinDeadline(t *testing.T) {
	host, port := startSlowSSHServer(t, 10*time.Millisecond)
	c := newSlowTestClient(t, host, port)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stdout, _, err := c.Execute(ctx, "echo hi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stdout != "done" {
		t.Errorf("want stdout %q, got %q", "done", stdout)
	}
}
