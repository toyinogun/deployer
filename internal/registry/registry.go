// Package registry is the platform's read side of the in cluster image
// registry. It resolves the tag a build pushed to the digest every deploy runs
// by, and reads the pushed image's config so a root image is refused before any
// workload is composed.
//
// Deliberately small: two reads over the distribution v2 HTTP API, no client
// library, no image manipulation. The platform never pushes or pulls here.
package registry

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

// The media types a registry may answer a manifest request with. Asking for all
// of them means one request covers a plain image and a multi platform index.
const acceptManifests = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// maxConfigBytes caps the config blob read. An image config is a small JSON
// document; anything near this is something other than what it claims to be.
const maxConfigBytes = 1 << 20

// requestTimeout bounds one registry call, so a wedged registry fails as itself
// rather than by consuming the whole deploy budget.
const requestTimeout = 30 * time.Second

// ErrNoDigest means the tag resolved to nothing. A build that reported success
// and left no manifest behind is a failed build, not a puzzle to retry.
var ErrNoDigest = errors.New("registry: tag resolves to no manifest")

// Client reads one registry over plain HTTP inside the cluster.
type Client struct {
	host     string // host and port, no scheme
	user     string
	password string
	http     *http.Client
}

// New returns a client for the registry at host, authenticating with Basic auth
// the way distribution's htpasswd backend expects.
func New(host, user, password string) *Client {
	return &Client{
		host:     host,
		user:     user,
		password: password,
		http:     &http.Client{Timeout: requestTimeout},
	}
}

// Digest resolves a repository and tag to the digest of the manifest behind it.
//
// This is where the platform learns what a build produced. The build container
// reports nothing: it pushes, and the registry is asked what landed, which
// removes a wrapper image and a report parser from the build path entirely.
func (c *Client) Digest(ctx context.Context, repo, tag string) (string, error) {
	resp, err := c.do(ctx, http.MethodHead, c.manifestURL(repo, tag), acceptManifests)
	if err != nil {
		return "", err
	}
	defer c.drain(resp)

	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNoDigest
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry: resolving %s:%s answered %d", repo, tag, resp.StatusCode)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", ErrNoDigest
	}
	return digest, nil
}

// ImageUser returns the user an image runs as, read from its config blob.
//
// An empty string means the image names no user, which is the same problem as
// naming root: the caller refuses either. This is the last gate before a
// workload is composed, and it deliberately runs against the digest the platform
// just resolved rather than against a tag anything could have moved.
func (c *Client) ImageUser(ctx context.Context, repo, digest string) (string, error) {
	configDigest, err := c.configDigest(ctx, repo, digest)
	if err != nil {
		return "", err
	}

	resp, err := c.do(ctx, http.MethodGet, c.blobURL(repo, configDigest), "")
	if err != nil {
		return "", err
	}
	defer c.drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry: reading the config of %s answered %d", repo, resp.StatusCode)
	}

	var config struct {
		Config struct {
			User string `json:"User"`
		} `json:"config"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxConfigBytes)).Decode(&config); err != nil {
		return "", fmt.Errorf("registry: decoding the config of %s: %w", repo, err)
	}
	return strings.TrimSpace(config.Config.User), nil
}

// configDigest follows a manifest to the config blob it names, stepping through
// one level of index when the registry answered with a multi platform list.
func (c *Client) configDigest(ctx context.Context, repo, reference string) (string, error) {
	var manifest struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := c.manifest(ctx, repo, reference, &manifest); err != nil {
		return "", err
	}
	if manifest.Config.Digest != "" {
		return manifest.Config.Digest, nil
	}
	// An index, so follow its first entry. The platform builds for one platform,
	// so an index here has exactly one thing worth following.
	if len(manifest.Manifests) == 0 {
		return "", ErrNoDigest
	}
	if err := c.manifest(ctx, repo, manifest.Manifests[0].Digest, &manifest); err != nil {
		return "", err
	}
	if manifest.Config.Digest == "" {
		return "", ErrNoDigest
	}
	return manifest.Config.Digest, nil
}

// manifest fetches one manifest and decodes it into out.
func (c *Client) manifest(ctx context.Context, repo, reference string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, c.manifestURL(repo, reference), acceptManifests)
	if err != nil {
		return err
	}
	defer c.drain(resp)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoDigest
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry: reading %s@%s answered %d", repo, reference, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxConfigBytes)).Decode(out); err != nil {
		return fmt.Errorf("registry: decoding the manifest of %s: %w", repo, err)
	}
	return nil
}

// do issues one authenticated request.
func (c *Client) do(ctx context.Context, method, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: preparing a request: %w", err)
	}
	req.SetBasicAuth(c.user, c.password)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: %s %s: %w", method, url, err)
	}
	return resp, nil
}

// drain closes a response body, reading the remainder first so the connection
// can be reused rather than torn down.
func (c *Client) drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxConfigBytes))
	_ = resp.Body.Close()
}

// manifestURL is the manifest endpoint for a reference, which may be a tag or a
// digest. The repository is platform derived (it is built from the app's slug),
// but it is escaped anyway rather than trusted by provenance.
func (c *Client) manifestURL(repo, reference string) string {
	return c.base() + "/v2/" + repoPath(repo) + "/manifests/" + url.PathEscape(reference)
}

// blobURL is the blob endpoint for a digest.
func (c *Client) blobURL(repo, digest string) string {
	return c.base() + "/v2/" + repoPath(repo) + "/blobs/" + url.PathEscape(digest)
}

// base is the registry's address. Plain HTTP: the registry has no Ingress, is
// reachable only on the pod network, and holds nothing worth a certificate the
// cluster would then have to rotate (spec 0004, Security model).
func (c *Client) base() string { return "http://" + c.host }

// repoPath drops the host from a fully qualified repository and escapes each
// path segment, since a repository name legitimately contains slashes.
func repoPath(repo string) string {
	repo = strings.TrimPrefix(repo, "http://")
	if host, rest, ok := strings.Cut(repo, "/"); ok && strings.ContainsAny(host, ".:") {
		repo = rest
	}
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
