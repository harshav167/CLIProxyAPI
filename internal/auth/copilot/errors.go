package copilot

import (
	"errors"
	"fmt"
	"net/http"
)

// OAuthError represents an OAuth-specific error.
type OAuthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	URI         string `json:"error_uri,omitempty"`
	StatusCode  int    `json:"-"`
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("OAuth error %s: %s", e.Code, e.Description)
	}
	return fmt.Sprintf("OAuth error: %s", e.Code)
}

func NewOAuthError(code, description string, statusCode int) *OAuthError {
	return &OAuthError{
		Code:        code,
		Description: description,
		StatusCode:  statusCode,
	}
}

// AuthenticationError represents authentication-related errors.
type AuthenticationError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    int    `json:"code"`
	Cause   error  `json:"-"`
}

func (e *AuthenticationError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (caused by: %v)", e.Type, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

func (e *AuthenticationError) Unwrap() error {
	return e.Cause
}

var (
	ErrDeviceCodeFailed = &AuthenticationError{
		Type:    "device_code_failed",
		Message: "Failed to request device code from GitHub",
		Code:    http.StatusBadRequest,
	}
	ErrDeviceCodeExpired = &AuthenticationError{
		Type:    "device_code_expired",
		Message: "Device code has expired. Please try again.",
		Code:    http.StatusGone,
	}
	ErrAuthorizationPending = &AuthenticationError{
		Type:    "authorization_pending",
		Message: "Authorization is pending. Waiting for user to authorize.",
		Code:    http.StatusAccepted,
	}
	ErrSlowDown = &AuthenticationError{
		Type:    "slow_down",
		Message: "Polling too frequently. Slowing down.",
		Code:    http.StatusTooManyRequests,
	}
	ErrAccessDenied = &AuthenticationError{
		Type:    "access_denied",
		Message: "User denied authorization",
		Code:    http.StatusForbidden,
	}
	ErrTokenExchangeFailed = &AuthenticationError{
		Type:    "token_exchange_failed",
		Message: "Failed to exchange device code for access token",
		Code:    http.StatusBadRequest,
	}
	ErrPollingTimeout = &AuthenticationError{
		Type:    "polling_timeout",
		Message: "Timeout waiting for user authorization",
		Code:    http.StatusRequestTimeout,
	}
	ErrUserInfoFailed = &AuthenticationError{
		Type:    "user_info_failed",
		Message: "Failed to fetch GitHub user information",
		Code:    http.StatusBadRequest,
	}
)

func NewAuthenticationError(baseErr *AuthenticationError, cause error) *AuthenticationError {
	return &AuthenticationError{
		Type:    baseErr.Type,
		Message: baseErr.Message,
		Code:    baseErr.Code,
		Cause:   cause,
	}
}

func IsAuthenticationError(err error) bool {
	var authenticationError *AuthenticationError
	return errors.As(err, &authenticationError)
}

func IsOAuthError(err error) bool {
	var oAuthError *OAuthError
	return errors.As(err, &oAuthError)
}

func GetUserFriendlyMessage(err error) string {
	var authErr *AuthenticationError
	if errors.As(err, &authErr) {
		switch authErr.Type {
		case "device_code_failed":
			return "Failed to start GitHub authentication. Please check your network connection and try again."
		case "device_code_expired":
			return "The authentication code has expired. Please try again."
		case "authorization_pending":
			return "Waiting for you to authorize the application on GitHub."
		case "slow_down":
			return "Please wait a moment before trying again."
		case "access_denied":
			return "Authentication was cancelled or denied."
		case "token_exchange_failed":
			return "Failed to complete authentication. Please try again."
		case "polling_timeout":
			return "Authentication timed out. Please try again."
		case "user_info_failed":
			return "Failed to get your GitHub account information. Please try again."
		default:
			return "Authentication failed. Please try again."
		}
	}

	var oauthErr *OAuthError
	if errors.As(err, &oauthErr) {
		switch oauthErr.Code {
		case "access_denied":
			return "Authentication was cancelled or denied."
		case "invalid_request":
			return "Invalid authentication request. Please try again."
		case "server_error":
			return "GitHub server error. Please try again later."
		default:
			return fmt.Sprintf("Authentication failed: %s", oauthErr.Description)
		}
	}

	return "An unexpected error occurred. Please try again."
}
