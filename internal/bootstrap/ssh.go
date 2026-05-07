package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// HostKeyDecision is returned by HostKeyPrompt to tell the SSH layer
// whether to accept and persist a freshly seen host key.
type HostKeyDecision int

const (
	HostKeyReject HostKeyDecision = iota
	HostKeyAccept
)

// HostKeyPrompt is invoked the first time a host key is seen for a given
// host. Implementations should display the fingerprint to the user and
// return their decision. A nil prompt rejects all unknown keys.
type HostKeyPrompt func(ctx context.Context, addr string, key ssh.PublicKey) (HostKeyDecision, error)

// Client is a thin wrapper over *ssh.Client that adds context-aware Run
// and Stream helpers. It is safe to call Run/Stream concurrently from
// multiple goroutines (each opens its own session).
type Client struct {
	conn *ssh.Client
	node Node
}

// Dial opens an SSH connection to the node using the configured auth
// method and verifies the host key against the supplied store.
func Dial(ctx context.Context, node Node, store *KnownHosts, prompt HostKeyPrompt) (*Client, error) {
	auths, err := buildAuths(node)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            node.User,
		Auth:            auths,
		HostKeyCallback: hostKeyCallback(ctx, store, prompt),
		Timeout:         10 * time.Second,
	}
	addr := net.JoinHostPort(node.Host, strconv.Itoa(portOrDefault(node.Port)))

	type result struct {
		c   *ssh.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ssh.Dial("tcp", addr, cfg)
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("dial %s: %w", addr, r.err)
		}
		return &Client{conn: r.c, node: node}, nil
	}
}

func portOrDefault(p int) int {
	if p == 0 {
		return 22
	}
	return p
}

// Close releases the underlying TCP connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Node returns the node descriptor this client connected to.
func (c *Client) Node() Node { return c.node }

// Run executes the command on the remote host and returns stdout, stderr,
// the exit code, and any transport error. A non-zero exit code is NOT a
// transport error — err will be nil and code will be set.
func (c *Client) Run(ctx context.Context, cmd string) (stdout, stderr string, code int, err error) {
	var outBuf, errBuf bytes.Buffer
	code, err = c.runStreaming(ctx, cmd, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), code, err
}

// Stream executes the command and invokes onLine for every newline-
// terminated line of stdout ("stdout") and stderr ("stderr"). The line
// passed to the callback does NOT include the trailing newline.
func (c *Client) Stream(ctx context.Context, cmd string, onLine func(stream, line string)) (int, error) {
	out := &lineWriter{stream: "stdout", fn: onLine}
	err := &lineWriter{stream: "stderr", fn: onLine}
	defer out.flush()
	defer err.flush()
	return c.runStreaming(ctx, cmd, out, err)
}

func (c *Client) runStreaming(ctx context.Context, cmd string, out, errw io.Writer) (int, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return -1, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	sess.Stdout = out
	sess.Stderr = errw

	if err := sess.Start(cmd); err != nil {
		return -1, fmt.Errorf("start: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		// Force-close so Wait unblocks even if the remote ignores SIGTERM.
		_ = sess.Close()
		<-done
		return -1, ctx.Err()
	case werr := <-done:
		if werr == nil {
			return 0, nil
		}
		var ee *ssh.ExitError
		if errors.As(werr, &ee) {
			return ee.ExitStatus(), nil
		}
		var me *ssh.ExitMissingError
		if errors.As(werr, &me) {
			return -1, fmt.Errorf("exit status missing: %w", werr)
		}
		return -1, werr
	}
}

func buildAuths(n Node) ([]ssh.AuthMethod, error) {
	switch n.Auth {
	case AuthPassword:
		return []ssh.AuthMethod{ssh.Password(n.Password)}, nil
	case AuthPrivateKey:
		var data []byte
		if len(n.PrivateKeyData) > 0 {
			data = n.PrivateKeyData
		} else if n.PrivateKeyPath != "" {
			d, err := readPrivateKeyFile(n.PrivateKeyPath)
			if err != nil {
				return nil, err
			}
			data = d
		} else {
			return nil, errors.New("private key auth selected but no key supplied")
		}
		signer, err := parsePrivateKey(data, n.Password)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case AuthAgent:
		return agentAuths()
	default:
		return nil, fmt.Errorf("unknown auth method %q", n.Auth)
	}
}

func readPrivateKeyFile(path string) ([]byte, error) {
	if strings.HasSuffix(path, ".pub") {
		privatePath := strings.TrimSuffix(path, ".pub")
		if info, err := os.Stat(privatePath); err == nil && !info.IsDir() {
			path = privatePath
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("check matching private key: %w", err)
		} else {
			return nil, fmt.Errorf("public key selected (%s); choose the matching private key file (%s)", path, privatePath)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	return data, nil
}

func parsePrivateKey(data []byte, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	}
	return ssh.ParsePrivateKey(data)
}

func agentAuths() ([]ssh.AuthMethod, error) {
	var auths []ssh.AuthMethod
	var reasons []string

	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		reasons = append(reasons, "SSH_AUTH_SOCK is not set")
	} else {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("dial ssh-agent: %v", err))
		} else {
			ag := agent.NewClient(conn)
			signers, err := ag.Signers()
			if err != nil {
				reasons = append(reasons, fmt.Sprintf("list ssh-agent keys: %v", err))
			} else if len(signers) == 0 {
				reasons = append(reasons, "ssh-agent has no keys loaded")
			} else {
				auths = append(auths, ssh.PublicKeys(signers...))
			}
		}
	}

	keyAuths, keyReasons := defaultIdentityAuths()
	auths = append(auths, keyAuths...)
	reasons = append(reasons, keyReasons...)
	if len(auths) > 0 {
		return auths, nil
	}

	if len(reasons) == 0 {
		reasons = append(reasons, "no default private keys found")
	}
	return nil, fmt.Errorf("ssh-agent auth selected but no usable keys were found (%s)", strings.Join(reasons, "; "))
}

func defaultIdentityAuths() ([]ssh.AuthMethod, []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, []string{fmt.Sprintf("find home directory: %v", err)}
	}

	var auths []ssh.AuthMethod
	var reasons []string
	for _, path := range defaultIdentityFiles(home) {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("read %s: %v", path, err))
			continue
		}
		signer, err := parsePrivateKey(data, "")
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("parse %s: %v", path, err))
			continue
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}

	if len(auths) == 0 && len(reasons) == 0 {
		reasons = append(reasons, fmt.Sprintf("no default private keys found in %s", filepath.Join(home, ".ssh")))
	}
	return auths, reasons
}

func defaultIdentityFiles(home string) []string {
	sshDir := filepath.Join(home, ".ssh")
	return []string{
		filepath.Join(sshDir, "id_ed25519"),
		filepath.Join(sshDir, "id_ecdsa"),
		filepath.Join(sshDir, "id_rsa"),
		filepath.Join(sshDir, "id_dsa"),
	}
}

// lineWriter buffers bytes and emits complete lines to fn. The last
// partial line (if any) is emitted by flush().
type lineWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	stream string
	fn     func(stream, line string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, _ := w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := w.buf.Next(i + 1)
		// strip trailing \r\n or \n
		end := len(line) - 1
		if end >= 0 && line[end] == '\n' {
			end--
		}
		if end >= 0 && line[end] == '\r' {
			end--
		}
		w.fn(w.stream, string(line[:end+1]))
	}
	return n, nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.fn(w.stream, w.buf.String())
		w.buf.Reset()
	}
}
