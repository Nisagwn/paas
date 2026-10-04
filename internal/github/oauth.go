package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Faz 13: GitHub OAuth App login (authorization code flow with PKCE) and
// the user/org lookups that authorization needs.

// DefaultWebURL is where users authorize OAuth Apps. GitHub Enterprise
// Server: https://<host>.
const DefaultWebURL = "https://github.com"

// OAuthApp is a GitHub OAuth App's credentials.
type OAuthApp struct {
	ClientID     string
	ClientSecret string
	// WebURL hosts /login/oauth/*; empty means DefaultWebURL.
	WebURL string
	HTTP   *http.Client
}

func (o *OAuthApp) web() string {
	if o.WebURL == "" {
		return DefaultWebURL
	}
	return strings.TrimRight(o.WebURL, "/")
}

// AuthorizeURL is where the browser is sent to log in. challenge is the
// PKCE S256 challenge; scope may be empty (public profile only).
func (o *OAuthApp) AuthorizeURL(redirectURI, state, challenge, scope string) string {
	q := url.Values{
		"client_id": {o.ClientID}, "redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "allow_signup": {"false"},
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	return o.web() + "/login/oauth/authorize?" + q.Encode()
}

// Exchange trades an authorization code for a user access token.
func (o *OAuthApp) Exchange(ctx context.Context, code, redirectURI, verifier string) (string, error) {
	form := url.Values{
		"client_id": {o.ClientID}, "client_secret": {o.ClientSecret}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.web()+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("github oauth: token exchange: %w", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return "", fmt.Errorf("github oauth: token exchange: %d: %w", resp.StatusCode, err)
	}
	// GitHub reports errors such as bad_verification_code with status 200.
	if body.Error != "" || body.AccessToken == "" {
		return "", fmt.Errorf("github oauth: token exchange: %d %s %s", resp.StatusCode, body.Error, body.Description)
	}
	return body.AccessToken, nil
}

// User is a GitHub account.
type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// ErrNotFound: the API answered 404.
var ErrNotFound = errors.New("github: not found")

// AuthenticatedUser returns the account the client's token belongs to.
func (c *Client) AuthenticatedUser(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, c.Token, http.MethodGet, "/user", nil, &u)
	return u, err
}

// GetUser looks up an account by login.
func (c *Client) GetUser(ctx context.Context, login string) (User, error) {
	var u User
	err := c.do(ctx, c.Token, http.MethodGet, "/users/"+url.PathEscape(login), nil, &u)
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return u, ErrNotFound
	}
	return u, err
}

// UserOrgs returns the logins of the organizations of the token's user.
// Private memberships need the read:org scope.
func (c *Client) UserOrgs(ctx context.Context) ([]string, error) {
	var out []string
	path := "/user/orgs?per_page=100"
	for page := 0; path != "" && page < 10; page++ {
		var orgs []struct {
			Login string `json:"login"`
		}
		next, err := c.request(ctx, c.Token, http.MethodGet, path, nil, &orgs)
		if err != nil {
			return nil, err
		}
		for _, o := range orgs {
			out = append(out, o.Login)
		}
		path = next
	}
	return out, nil
}
