// Package github implements GitHub App authentication and a Git Data REST
// client satisfying the replay.Client interface.
package github

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/bradleyfalzon/ghinstallation/v2"
)

// NewAppTransport builds an http.RoundTripper that authenticates as the
// given GitHub App installation, minting and refreshing installation
// tokens as needed.
func NewAppTransport(appID, installationID int64, privateKeyPath string) (http.RoundTripper, error) {
	tr, err := ghinstallation.NewKeyFromFile(http.DefaultTransport, appID, installationID, privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("github: build app transport: %w", err)
	}
	return tr, nil
}

// Token returns the current installation token held by a transport built
// with NewAppTransport, minting or refreshing it if necessary.
func Token(ctx context.Context, transport http.RoundTripper) (string, error) {
	t, ok := transport.(*ghinstallation.Transport)
	if !ok {
		return "", fmt.Errorf("github: transport %T does not expose an installation token", transport)
	}
	return t.Token(ctx)
}

// FromEnv reads GITHUB_APP_ID, GITHUB_APP_INSTALLATION_ID, and
// GITHUB_APP_PRIVATE_KEY_PATH. The error names the missing or non-numeric
// variable.
func FromEnv() (appID, installationID int64, keyPath string, err error) {
	appID, err = envInt64("GITHUB_APP_ID")
	if err != nil {
		return 0, 0, "", err
	}
	installationID, err = envInt64("GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return 0, 0, "", err
	}
	keyPath = os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	if keyPath == "" {
		return 0, 0, "", fmt.Errorf("github: missing required environment variable: GITHUB_APP_PRIVATE_KEY_PATH")
	}
	return appID, installationID, keyPath, nil
}

func envInt64(name string) (int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return 0, fmt.Errorf("github: missing required environment variable: %s", name)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("github: environment variable %s must be numeric: %q", name, v)
	}
	if n <= 0 {
		return 0, fmt.Errorf("github: environment variable %s must be a positive integer: %q", name, v)
	}
	return n, nil
}
