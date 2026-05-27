package helps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GetClaudeClientUserAgent extracts the client User-Agent from the gin context.
func GetClaudeClientUserAgent(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.GetHeader("User-Agent")
	}
	return ""
}

// ParseClaudeEntrypointFromUA extracts the entrypoint from a Claude Code User-Agent.
func ParseClaudeEntrypointFromUA(userAgent string) string {
	start := strings.Index(userAgent, "(")
	end := strings.LastIndex(userAgent, ")")
	if start < 0 || end <= start {
		return "cli"
	}
	inner := userAgent[start+1 : end]
	parts := strings.Split(inner, ",")
	if len(parts) >= 2 {
		ep := strings.TrimSpace(parts[1])
		if ep != "" {
			return ep
		}
	}
	return "cli"
}

// GetClaudeWorkloadFromContext extracts workload identifier from the gin
// request headers (X-CPA-Claude-Workload).
func GetClaudeWorkloadFromContext(ctx context.Context) string {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return strings.TrimSpace(ginCtx.GetHeader("X-CPA-Claude-Workload"))
	}
	return ""
}

// GetClaudeCloakConfigFromAuth extracts cloak configuration from auth attributes.
func GetClaudeCloakConfigFromAuth(auth *cliproxyauth.Auth) (string, bool, []string, bool) {
	if auth == nil || auth.Attributes == nil {
		return "auto", false, nil, false
	}

	cloakMode := auth.Attributes["cloak_mode"]
	if cloakMode == "" {
		cloakMode = "auto"
	}

	strictMode := strings.ToLower(auth.Attributes["cloak_strict_mode"]) == "true"

	var sensitiveWords []string
	if wordsStr := auth.Attributes["cloak_sensitive_words"]; wordsStr != "" {
		sensitiveWords = strings.Split(wordsStr, ",")
		for i := range sensitiveWords {
			sensitiveWords[i] = strings.TrimSpace(sensitiveWords[i])
		}
	}

	cacheUserID := strings.EqualFold(strings.TrimSpace(auth.Attributes["cloak_cache_user_id"]), "true")

	return cloakMode, strictMode, sensitiveWords, cacheUserID
}

// InjectClaudeFakeUserID generates and injects a fake user ID into the request
// metadata. When useCache is false, a new user ID is generated for every call.
func InjectClaudeFakeUserID(payload []byte, apiKey string, useCache bool) []byte {
	generateID := func() string {
		if useCache {
			return CachedUserID(apiKey)
		}
		return GenerateFakeUserID()
	}

	metadata := gjson.GetBytes(payload, "metadata")
	if !metadata.Exists() {
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", generateID())
		return payload
	}

	existingUserID := gjson.GetBytes(payload, "metadata.user_id").String()
	if existingUserID == "" || !IsValidUserID(existingUserID) {
		payload, _ = sjson.SetBytes(payload, "metadata.user_id", generateID())
	}
	return payload
}

const claudeFingerprintSalt = "59cf53e54c78"

// ComputeClaudeFingerprint computes the 3-char build fingerprint that Claude
// Code embeds in cc_version.
func ComputeClaudeFingerprint(messageText, version string) string {
	indices := [3]int{4, 7, 20}
	runes := []rune(messageText)
	var sb strings.Builder
	for _, idx := range indices {
		if idx < len(runes) {
			sb.WriteRune(runes[idx])
		} else {
			sb.WriteRune('0')
		}
	}
	input := claudeFingerprintSalt + sb.String() + version
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])[:3]
}

// GenerateClaudeBillingHeader creates the x-anthropic-billing-header text
// block that real Claude Code prepends to every system prompt array.
func GenerateClaudeBillingHeader(payload []byte, experimentalCCHSigning bool, version, messageText, entrypoint, workload string) string {
	if entrypoint == "" {
		entrypoint = "cli"
	}
	buildHash := ComputeClaudeFingerprint(messageText, version)
	workloadPart := ""
	if workload != "" {
		workloadPart = fmt.Sprintf(" cc_workload=%s;", workload)
	}

	if experimentalCCHSigning {
		return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=00000;%s", version, buildHash, entrypoint, workloadPart)
	}

	h := sha256.Sum256(payload)
	cch := hex.EncodeToString(h[:])[:5]
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=%s; cch=%s;%s", version, buildHash, entrypoint, cch, workloadPart)
}
