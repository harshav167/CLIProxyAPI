package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// GitHubCopilotAuthenticator implements the OAuth device flow login for GitHub Copilot.
type GitHubCopilotAuthenticator struct{}

func NewGitHubCopilotAuthenticator() Authenticator {
	return &GitHubCopilotAuthenticator{}
}

func (GitHubCopilotAuthenticator) Provider() string {
	return "github-copilot"
}

func (GitHubCopilotAuthenticator) RefreshLead() *time.Duration {
	return nil
}

func (a GitHubCopilotAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := copilot.NewCopilotAuth(cfg)

	fmt.Println("Starting GitHub Copilot authentication...")
	deviceCode, err := authSvc.StartDeviceFlow(ctx)
	if err != nil {
		return nil, fmt.Errorf("github-copilot: failed to start device flow: %w", err)
	}

	fmt.Printf("\nTo authenticate, please visit: %s\n", deviceCode.VerificationURI)
	fmt.Printf("And enter the code: %s\n\n", deviceCode.UserCode)

	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(deviceCode.VerificationURI); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
			}
		}
	}

	fmt.Println("Waiting for GitHub authorization...")
	fmt.Printf("(This will timeout in %d seconds if not authorized)\n", deviceCode.ExpiresIn)

	authBundle, err := authSvc.WaitForAuthorization(ctx, deviceCode)
	if err != nil {
		errMsg := copilot.GetUserFriendlyMessage(err)
		return nil, fmt.Errorf("github-copilot: %s", errMsg)
	}

	fmt.Println("Verifying Copilot access...")
	apiToken, err := authSvc.GetCopilotAPIToken(ctx, authBundle.TokenData.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("github-copilot: failed to verify Copilot access - you may not have an active Copilot subscription: %w", err)
	}

	tokenStorage := authSvc.CreateTokenStorage(authBundle)

	metadata := map[string]any{
		"type":         "github-copilot",
		"username":     authBundle.Username,
		"email":        authBundle.Email,
		"name":         authBundle.Name,
		"access_token": authBundle.TokenData.AccessToken,
		"token_type":   authBundle.TokenData.TokenType,
		"scope":        authBundle.TokenData.Scope,
		"timestamp":    time.Now().UnixMilli(),
	}

	if apiToken.ExpiresAt > 0 {
		metadata["api_token_expires_at"] = apiToken.ExpiresAt
	}

	fileName := fmt.Sprintf("github-copilot-%s.json", authBundle.Username)

	label := authBundle.Email
	if label == "" {
		label = authBundle.Username
	}

	fmt.Printf("\nGitHub Copilot authentication successful for user: %s\n", authBundle.Username)

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    label,
		Storage:  tokenStorage,
		Metadata: metadata,
	}, nil
}

func RefreshGitHubCopilotToken(ctx context.Context, cfg *config.Config, storage *copilot.CopilotTokenStorage) error {
	if storage == nil || storage.AccessToken == "" {
		return fmt.Errorf("no token available")
	}

	authSvc := copilot.NewCopilotAuth(cfg)
	_, err := authSvc.GetCopilotAPIToken(ctx, storage.AccessToken)
	if err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}

	return nil
}
