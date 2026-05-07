package bootstrap

import (
	"strings"
	"testing"
)

func TestRedactCommandHidesBecomePasswordAndToken(t *testing.T) {
	node := NewNode(RoleAgent)
	node.Password = "ssh-secret"
	node.BecomePassword = "sudo secret"
	cmd := "printf '%s\n' 'sudo secret' | sudo -S bash -c 'K3S_TOKEN=cluster-token echo ssh-secret'"

	got := redactCommand(cmd, node, "cluster-token")
	for _, secret := range []string{"sudo secret", "ssh-secret", "cluster-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted command still contains %q: %s", secret, got)
		}
	}
	if strings.Count(got, "[redacted]") == 0 {
		t.Fatalf("redacted command missing marker: %s", got)
	}
}
