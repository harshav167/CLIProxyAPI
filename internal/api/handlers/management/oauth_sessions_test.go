package management

import "testing"

func TestNormalizeOAuthProviderAcceptsGitHubAlias(t *testing.T) {
	got, err := NormalizeOAuthProvider("github")
	if err != nil {
		t.Fatalf("NormalizeOAuthProvider returned error: %v", err)
	}
	if got != "github-copilot" {
		t.Fatalf("NormalizeOAuthProvider = %q, want %q", got, "github-copilot")
	}
}

func TestNormalizeOAuthProviderAcceptsGitHubCopilotAlias(t *testing.T) {
	got, err := NormalizeOAuthProvider("github-copilot")
	if err != nil {
		t.Fatalf("NormalizeOAuthProvider returned error: %v", err)
	}
	if got != "github-copilot" {
		t.Fatalf("NormalizeOAuthProvider = %q, want %q", got, "github-copilot")
	}
}
