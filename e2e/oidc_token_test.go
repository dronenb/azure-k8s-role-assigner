package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// tokenClaims holds the claims extracted from an Entra ID (v2.0) token.
type tokenClaims struct {
	Groups []string `json:"groups"`
}

// ropcToken performs a Resource Owner Password Credentials grant against the
// Microsoft identity platform v2.0 token endpoint and returns the raw token
// response. This mirrors the flow previously handled by the kubelogin CLI and
// the inline curl in the Taskfile, but in pure Go with no external binaries.
func ropcToken(ctx context.Context, tenantID, clientID, scope, username, password string) (map[string]any, error) {
	endpoint := "https://login.microsoftonline.com/" + url.PathEscape(tenantID) + "/oauth2/v2.0/token"

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "password")
	form.Set("scope", scope)
	form.Set("username", username)
	form.Set("password", password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build ROPC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ROPC token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read ROPC token response: %w", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("ROPC response should be JSON: %w: %s", err, string(body))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ROPC token request returned %s: %s", resp.Status, string(body))
	}
	return claims, nil
}

// ropcAccessTokenGroups obtains an access token whose audience is clientID and
// returns the group claims. The scope uses <clientID>/.default to target the
// given app registration.
func ropcAccessTokenGroups(ctx context.Context, cfg e2eConfig, clientID string) ([]string, error) {
	scope := clientID + "/.default openid profile"
	claims, err := ropcToken(ctx, cfg.tenantID, clientID, scope, cfg.userUPN, cfg.userPassword)
	if err != nil {
		return nil, err
	}

	token, ok := claims["access_token"].(string)
	if !ok || token == "" {
		return nil, fmt.Errorf("ROPC response should include an access_token")
	}
	return decodeGroups(token)
}

// ropcIDTokenGroups obtains an ID token (openid/profile/email scopes) and
// returns the group claims, used to verify the Argo CD OIDC integration.
func ropcIDTokenGroups(ctx context.Context, cfg e2eConfig, clientID string) ([]string, error) {
	claims, err := ropcToken(ctx, cfg.tenantID, clientID, "openid profile email", cfg.userUPN, cfg.userPassword)
	if err != nil {
		return nil, err
	}

	token, ok := claims["id_token"].(string)
	if !ok || token == "" {
		return nil, fmt.Errorf("ROPC response should include an id_token")
	}
	return decodeGroups(token)
}

// decodeGroups parses the `.groups` claim out of a JWT (access or ID token).
func decodeGroups(token string) ([]string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("token should be a JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil, fmt.Errorf("JWT payload should be base64url encoded: %w", err)
	}

	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("JWT payload should be JSON: %w: %s", err, string(payload))
	}
	return claims.Groups, nil
}
