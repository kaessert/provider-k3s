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
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

const (
	errParsePrivateKey = "cannot parse SSH private key"
	errSSHDial         = "cannot connect to SSH server"
	errSSHSession      = "cannot create SSH session"
	errSSHExecute      = "cannot execute SSH command"
)

// Config holds SSH connection parameters.
type Config struct {
	Host       string
	Port       int
	Username   string
	Password   string // for password-based auth
	PrivateKey []byte // for key-based auth (PEM-encoded)
}

// Client wraps an SSH connection for remote command execution.
type Client struct {
	conn *ssh.Client
}

// NewClient establishes an SSH connection using the provided config.
// It tries key-based auth first (if PrivateKey is set), then falls back to password.
func NewClient(cfg Config) (*Client, error) {
	var authMethods []ssh.AuthMethod

	if len(cfg.PrivateKey) > 0 {
		signer, err := ssh.ParsePrivateKey(cfg.PrivateKey)
		if err != nil {
			return nil, errors.Wrap(err, errParsePrivateKey)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if cfg.Password != "" {
		authMethods = append(authMethods, ssh.Password(cfg.Password))
	}

	sshConfig := &ssh.ClientConfig{
		User:            cfg.Username,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // same as k3sup
	}

	address := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	conn, err := ssh.Dial("tcp", address, sshConfig)
	if err != nil {
		return nil, errors.Wrap(err, errSSHDial)
	}

	return &Client{conn: conn}, nil
}

// Execute runs a command on the remote host and returns stdout and stderr.
// It honours ctx: if ctx is done before the remote command finishes, Execute
// closes the SSH session (ending the blocking wait promptly) and returns
// ctx.Err() rather than blocking until the command completes on its own.
// Without this, a slow remote command (a multi-minute k3s install, for
// example) blocks past the caller's deadline, and by the time it does
// return, every context-aware call the caller makes next (a Kubernetes API
// write to persist the result) fails immediately too -- because the SAME,
// already-expired context is still what's guarding those calls.
func (c *Client) Execute(ctx context.Context, cmd string) (stdout, stderr string, err error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return "", "", errors.Wrap(err, errSSHSession)
	}
	defer session.Close() //nolint:errcheck // best-effort: either the done path or the ctx.Done() path already closed it

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	if err := session.Start(cmd); err != nil {
		return "", "", errors.Wrap(err, errSSHExecute)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case <-ctx.Done():
		// Close the session so the blocked Wait() goroutine above returns
		// and this call unblocks promptly instead of waiting for the
		// remote command to finish on its own schedule. Then wait for
		// that goroutine to actually finish before reading the buffers:
		// Wait() only returns once the stdout/stderr copy goroutines it
		// started have stopped writing to them, so reading any earlier
		// races with those writes.
		_ = session.Close()
		<-done
		return strings.TrimSpace(stdoutBuf.String()), strings.TrimSpace(stderrBuf.String()), errors.Wrap(ctx.Err(), errSSHExecute)
	case waitErr := <-done:
		if waitErr != nil {
			return strings.TrimSpace(stdoutBuf.String()), strings.TrimSpace(stderrBuf.String()), errors.Wrap(waitErr, errSSHExecute)
		}
		return strings.TrimSpace(stdoutBuf.String()), strings.TrimSpace(stderrBuf.String()), nil
	}
}

// ConfigureAuth sets the appropriate authentication on the Config
// based on the credential data. If the data looks like a PEM-encoded
// private key it is set as PrivateKey, otherwise as Password.
func (cfg *Config) ConfigureAuth(data []byte) {
	if bytes.HasPrefix(data, []byte("-----BEGIN")) {
		cfg.PrivateKey = data
	} else {
		cfg.Password = string(data)
	}
}

// Close terminates the SSH connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
