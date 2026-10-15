// Package registry deletes images the platform no longer keeps (Faz 30).
//
// The builder pushes every deployment's image as
// <registry>/<app>:<sha>-<deployment id> and the deployment pins it by
// digest. When the cleanup retires a deployment, its manifest is deleted
// through the Docker Registry HTTP API v2 / OCI distribution API:
//
//	HEAD   /v2/<repo>/manifests/<tag>     → Docker-Content-Digest (resolve)
//	DELETE /v2/<repo>/manifests/<digest>  → 202 (manifest and its tags gone)
//	GET    /v2/<repo>/tags/list           → tags (deleting a whole project)
//
// The same client serves the local registry (registry:2 with
// REGISTRY_STORAGE_DELETE_ENABLED=true) and Amazon ECR, which implements the
// OCI delete with the ecr:BatchDeleteImage permission. Credentials come from
// the Docker config the builder already uses ($DOCKER_CONFIG/config.json; on
// AWS the paas-ecr-auth timer refreshes it), read again on every call so a
// refreshed token is picked up at once. Both challenge schemes are handled:
// Basic (ECR) and Bearer token (Docker Hub, GHCR, Harbor, ...).
//
// Deciding WHICH images may go is the store's job (store.ReleaseImage): a
// digest is only deleted when no deployment that is still kept references
// it. This package only talks to the registry; every call is idempotent
// (a missing manifest or repository is not an error).
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrUnsupported: the registry refuses deletes (registry:2 without
// REGISTRY_STORAGE_DELETE_ENABLED, or a registry that does not implement
// the API). Retrying does not help; the image stays.
var ErrUnsupported = errors.New("registry does not allow deleting manifests")

// manifestTypes are accepted when resolving a tag. The digest must be the
// one of the manifest the tag points at (an image index for multi-platform
// or attested builds), so every type is listed.
var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

// Image is a parsed image reference "<host>/<repository>[:tag][@digest]".
type Image struct {
	Host       string // "localhost:5000", "123.dkr.ecr.eu-central-1.amazonaws.com"
	Repository string // "blog", "nisagwn/blog"
	Tag        string
	Digest     string // "sha256:…"
}

// Name is the reference without tag and digest: "<host>/<repository>".
func (i Image) Name() string { return i.Host + "/" + i.Repository }

func (i Image) String() string {
	s := i.Name()
	if i.Tag != "" {
		s += ":" + i.Tag
	}
	if i.Digest != "" {
		s += "@" + i.Digest
	}
	return s
}

// Parse splits an image reference. The first path component is the
// registry host; references without one (Docker Hub short names) are
// rejected, the platform always pushes with an explicit registry.
func Parse(ref string) (Image, error) {
	var img Image
	rest := ref
	if i := strings.Index(rest, "@"); i >= 0 {
		rest, img.Digest = rest[:i], rest[i+1:]
		if !validDigest(img.Digest) {
			return Image{}, fmt.Errorf("image %q: invalid digest", ref)
		}
	}
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		return Image{}, fmt.Errorf("image %q: no registry host", ref)
	}
	img.Host, rest = rest[:slash], rest[slash+1:]
	if !strings.ContainsAny(img.Host, ".:") && img.Host != "localhost" {
		return Image{}, fmt.Errorf("image %q: no registry host", ref)
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		rest, img.Tag = rest[:i], rest[i+1:]
	}
	img.Repository = rest
	if img.Repository == "" || (img.Tag == "" && img.Digest == "") {
		return Image{}, fmt.Errorf("image %q: no repository, tag or digest", ref)
	}
	return img, nil
}

func validDigest(d string) bool {
	alg, hex, ok := strings.Cut(d, ":")
	if !ok || alg == "" || len(hex) < 32 {
		return false
	}
	for _, c := range hex {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Credentials returns the user name and password for a registry host;
// empty strings mean anonymous access.
type Credentials func(host string) (user, password string, err error)

// Client talks to one registry.
type Client struct {
	// Host is the registry the platform pushes to ("localhost:5000").
	// Images on other hosts are never touched.
	Host string
	// Prefix is the path part of PAAS_REGISTRY ("nisagwn" in
	// "ghcr.io/nisagwn"); an app's repository is <prefix>/<app>.
	Prefix string
	// Insecure uses plain HTTP (PAAS_REGISTRY_INSECURE, or a loopback host).
	Insecure bool
	// Credentials; nil means anonymous.
	Credentials Credentials
	// HTTP defaults to a client with a 30s timeout.
	HTTP *http.Client
}

// New returns a client for the registry prefix of PAAS_REGISTRY
// ("localhost:5000", "ghcr.io/nisagwn", "<account>.dkr.ecr.<region>.amazonaws.com").
// Credentials come from the Docker config (DockerConfig).
func New(registryPrefix string, insecure bool) *Client {
	host, prefix, _ := strings.Cut(strings.TrimSuffix(registryPrefix, "/"), "/")
	return &Client{Host: host, Prefix: prefix, Insecure: insecure || loopback(host), Credentials: DockerConfig("")}
}

// AppRepository is the repository the builder pushes app's images to.
func (c *Client) AppRepository(app string) string {
	if c.Prefix == "" {
		return app
	}
	return c.Prefix + "/" + app
}

// DeleteAppImages deletes every image of a deleted project (Faz 28 step
// "cluster"). On ECR the empty repository itself stays; it is reused when a
// project of the same name is created.
func (c *Client) DeleteAppImages(ctx context.Context, app string) (int, error) {
	n, err := c.DeleteRepository(ctx, c.AppRepository(app))
	if errors.Is(err, ErrUnsupported) {
		return n, nil // deletes disabled: nothing this client can do
	}
	return n, err
}

func loopback(host string) bool {
	h := host
	if sh, _, err := net.SplitHostPort(host); err == nil {
		h = sh
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Owns reports whether img lives in this client's registry.
func (c *Client) Owns(img Image) bool { return strings.EqualFold(img.Host, c.Host) }

func (c *Client) base() string {
	scheme := "https"
	if c.Insecure {
		scheme = "http"
	}
	return scheme + "://" + c.Host + "/v2/"
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Resolve returns the digest img's tag points at, "" when the tag (or the
// repository) does not exist. An img with a digest returns it unchanged.
func (c *Client) Resolve(ctx context.Context, img Image) (string, error) {
	if img.Digest != "" {
		return img.Digest, nil
	}
	resp, err := c.do(ctx, http.MethodHead, img.Repository, "manifests/"+img.Tag, true)
	if err != nil {
		return "", err
	}
	defer drain(resp)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", nil
	case resp.StatusCode != http.StatusOK:
		return "", statusError("resolve "+img.String(), resp)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if !validDigest(d) {
		return "", fmt.Errorf("resolve %s: registry sent no digest", img)
	}
	return d, nil
}

// Delete deletes the manifest repository@digest; the registry removes every
// tag that points at it. A missing manifest is not an error.
func (c *Client) Delete(ctx context.Context, repository, digest string) error {
	if !validDigest(digest) {
		return fmt.Errorf("delete %s: invalid digest %q", repository, digest)
	}
	resp, err := c.do(ctx, http.MethodDelete, repository, "manifests/"+digest, false)
	if err != nil {
		return err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("delete %s@%s: %w", repository, digest, ErrUnsupported)
	}
	err = statusError("delete "+repository+"@"+digest, resp)
	if strings.Contains(err.Error(), "UNSUPPORTED") {
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return err
}

// Tags lists the repository's tags; a missing repository has none.
func (c *Client) Tags(ctx context.Context, repository string) ([]string, error) {
	var out []string
	next := "tags/list?n=1000"
	for next != "" {
		resp, err := c.do(ctx, http.MethodGet, repository, next, false)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			drain(resp)
			return out, nil
		}
		if resp.StatusCode != http.StatusOK {
			err := statusError("tags "+repository, resp)
			drain(resp)
			return nil, err
		}
		var page struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		link := resp.Header.Get("Link")
		drain(resp)
		if err != nil {
			return nil, fmt.Errorf("tags %s: %w", repository, err)
		}
		out = append(out, page.Tags...)
		next = nextPage(link, repository)
	}
	return out, nil
}

// DeleteRepository deletes every image of repository (a deleted project).
// The buildcache tag goes too. It returns how many manifests were deleted.
func (c *Client) DeleteRepository(ctx context.Context, repository string) (int, error) {
	tags, err := c.Tags(ctx, repository)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	n := 0
	for _, tag := range tags {
		d, err := c.Resolve(ctx, Image{Host: c.Host, Repository: repository, Tag: tag})
		if err != nil {
			return n, err
		}
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if err := c.Delete(ctx, repository, d); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// nextPage extracts the path after /v2/<repository>/ from a Link header:
// `</v2/blog/tags/list?last=x&n=1000>; rel="next"`.
func nextPage(link, repository string) string {
	if link == "" {
		return ""
	}
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start < 0 || end <= start {
		return ""
	}
	u, err := url.Parse(link[start+1 : end])
	if err != nil {
		return ""
	}
	prefix := "/v2/" + repository + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return ""
	}
	p := strings.TrimPrefix(u.Path, prefix)
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// do sends one request and answers an authentication challenge once.
func (c *Client) do(ctx context.Context, method, repository, path string, accept bool) (*http.Response, error) {
	target := c.base() + repository + "/" + path
	send := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, target, nil)
		if err != nil {
			return nil, err
		}
		if accept {
			req.Header.Set("Accept", strings.Join(manifestTypes, ", "))
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry %s: %w", c.Host, err)
		}
		return resp, nil
	}
	resp, err := send("")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	drain(resp)
	auth, err := c.authorize(ctx, challenge, repository)
	if err != nil {
		return nil, err
	}
	return send(auth)
}

// authorize answers a WWW-Authenticate challenge.
func (c *Client) authorize(ctx context.Context, challenge, repository string) (string, error) {
	user, pass, err := c.creds()
	if err != nil {
		return "", err
	}
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if user == "" && pass == "" {
			return "", fmt.Errorf("registry %s: authentication required, no credentials for it in the Docker config", c.Host)
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)), nil
	case "bearer":
		return c.token(ctx, params, repository, user, pass)
	}
	return "", fmt.Errorf("registry %s: unsupported authentication challenge %q", c.Host, challenge)
}

func (c *Client) creds() (string, string, error) {
	if c.Credentials == nil {
		return "", "", nil
	}
	return c.Credentials(c.Host)
}

// token fetches a bearer token for pull, push and delete on repository.
func (c *Client) token(ctx context.Context, params map[string]string, repository, user, pass string) (string, error) {
	realm := params["realm"]
	u, err := url.Parse(realm)
	if err != nil || realm == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("registry %s: invalid token realm %q", c.Host, realm)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+repository+":pull,push,delete")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("registry %s: token: %w", c.Host, err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", statusError("registry "+c.Host+": token", resp)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("registry %s: token: %w", c.Host, err)
	}
	t := tok.Token
	if t == "" {
		t = tok.AccessToken
	}
	if t == "" {
		return "", fmt.Errorf("registry %s: token: empty", c.Host)
	}
	return "Bearer " + t, nil
}

// parseChallenge splits `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		var val string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				val, rest = after[1:], ""
			} else {
				val, rest = after[1:1+end], after[2+end:]
			}
		} else {
			val, rest, _ = strings.Cut(after, ",")
		}
		params[key] = val
	}
	return scheme, params
}

// DockerConfig reads credentials from a Docker config.json: dir, else
// $DOCKER_CONFIG, else ~/.docker. Only inline "auths" entries are used
// (what docker login without a credential helper and the paas-ecr-auth
// timer write); a host without an entry is accessed anonymously.
func DockerConfig(dir string) Credentials {
	return func(host string) (string, string, error) {
		d := dir
		if d == "" {
			d = os.Getenv("DOCKER_CONFIG")
		}
		if d == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", "", nil
			}
			d = filepath.Join(home, ".docker")
		}
		b, err := os.ReadFile(filepath.Join(d, "config.json"))
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		if err != nil {
			return "", "", fmt.Errorf("docker config: %w", err)
		}
		var cfg struct {
			Auths map[string]struct {
				Auth     string `json:"auth"`
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"auths"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return "", "", fmt.Errorf("docker config: %w", err)
		}
		for key, a := range cfg.Auths {
			if !sameHost(key, host) {
				continue
			}
			if a.Auth != "" {
				raw, err := base64.StdEncoding.DecodeString(a.Auth)
				if err != nil {
					return "", "", fmt.Errorf("docker config: auth of %s: %w", key, err)
				}
				user, pass, _ := strings.Cut(string(raw), ":")
				return user, pass, nil
			}
			return a.Username, a.Password, nil
		}
		return "", "", nil
	}
}

// sameHost compares a config.json key ("https://host/v1/", "host") with host.
func sameHost(key, host string) bool {
	k := strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	k, _, _ = strings.Cut(k, "/")
	return strings.EqualFold(k, host)
}

func statusError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("%s: %s", what, resp.Status)
	}
	return fmt.Errorf("%s: %s: %s", what, resp.Status, msg)
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}
