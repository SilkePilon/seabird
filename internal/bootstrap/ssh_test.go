package bootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestBuildAuthsAgentUsesDefaultIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	sshDir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	auths, err := buildAuths(Node{Auth: AuthAgent})
	if err != nil {
		t.Fatalf("buildAuths: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("want one default identity auth, got %d", len(auths))
	}
}

func TestBuildAuthsAgentReportsNoUsableKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")

	_, err := buildAuths(Node{Auth: AuthAgent})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"no usable keys", "SSH_AUTH_SOCK is not set", "no default private keys"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestBuildAuthsPrivateKeyPathAcceptsPublicKeyCompanion(t *testing.T) {
	dir := t.TempDir()
	privatePath := filepath.Join(dir, "id_ed25519")
	writeTestPrivateKey(t, privatePath)
	if err := os.WriteFile(privatePath+".pub", []byte("ssh-ed25519 test\n"), 0o600); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	auths, err := buildAuths(Node{Auth: AuthPrivateKey, PrivateKeyPath: privatePath + ".pub"})
	if err != nil {
		t.Fatalf("buildAuths: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("want one private key auth, got %d", len(auths))
	}
}

func TestBuildAuthsPrivateKeyPathRejectsPublicKeyWithoutCompanion(t *testing.T) {
	publicPath := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(publicPath, []byte("ssh-ed25519 test\n"), 0o600); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	_, err := buildAuths(Node{Auth: AuthPrivateKey, PrivateKeyPath: publicPath})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"public key selected", "choose the matching private key file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func writeTestPrivateKey(t *testing.T, path string) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}
