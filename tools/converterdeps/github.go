package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// github is the GitHub REST API, as a GITHUB_TOKEN may use it: a workflow's
// own, or a person's. Without a token it reads only what is public.
type github struct {
	api, uploads string // https://api.github.com, https://uploads.github.com
	token        string
	hc           *http.Client
}

// errNotFound is a 404: no such release, asset or repository, or, without
// a token, one that is private.
var errNotFound = errors.New("not found")

type ghRelease struct {
	ID     int64     `json:"id"`
	Assets []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// Digest is "sha256:HEX", as GitHub computes it on upload.
	Digest string `json:"digest"`
}

type ghIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	HTMLURL     string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (g *github) do(method, u, accept string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, u, errNotFound)
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s %s: %s: %s", method, u, resp.Status, bytes.TrimSpace(b))
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o, err = io.ReadAll(resp.Body)
		return err
	default:
		return json.NewDecoder(resp.Body).Decode(out)
	}
}

// releaseByTag is the repository's release of tag.
func (g *github) releaseByTag(r release) (ghRelease, error) {
	var rel ghRelease
	err := g.do(http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", g.api, r.owner, r.repo, url.PathEscape(r.tag)),
		"application/vnd.github+json", nil, "", &rel)
	return rel, err
}

// createRelease makes the release of tag, the tag with it on the default
// branch: a pre-release never marked latest, so it is no one's release of
// the platform.
func (g *github) createRelease(r release) (ghRelease, error) {
	body, _ := json.Marshal(map[string]any{
		"tag_name": r.tag, "name": r.tag, "prerelease": true, "make_latest": "false",
		"body": "The Alpine packages the converter initramfs is built from, as guest/converter/converter.env pins them (docs/runbook/converter.md). Uploaded by the converter-mirror workflow; not a release of the platform.",
	})
	var rel ghRelease
	err := g.do(http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/releases", g.api, r.owner, r.repo),
		"application/vnd.github+json", bytes.NewReader(body), "application/json", &rel)
	return rel, err
}

// asset downloads an asset by its API URL, which a token can read in a
// private repository too.
func (g *github) asset(a ghAsset) ([]byte, error) {
	var b []byte
	err := g.do(http.MethodGet, a.URL, "application/octet-stream", nil, "", &b)
	return b, err
}

// upload adds data to the release as the asset name.
func (g *github) upload(r release, rel ghRelease, name string, data []byte) error {
	return g.do(http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/releases/%d/assets?name=%s", g.uploads, r.owner, r.repo, rel.ID, url.QueryEscape(name)),
		"application/vnd.github+json", bytes.NewReader(data), "application/octet-stream", nil)
}

// openIssue returns the repository's open issue titled title, if any.
func (g *github) openIssue(owner, repo, title string) (*ghIssue, error) {
	for page := 1; page <= 10; page++ {
		var issues []ghIssue
		if err := g.do(http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/issues?state=open&per_page=100&page=%d", g.api, owner, repo, page),
			"application/vnd.github+json", nil, "", &issues); err != nil {
			return nil, err
		}
		for i, is := range issues {
			// The issues list holds pull requests too, each with a
			// pull_request.
			isPR := len(is.PullRequest) > 0 && string(is.PullRequest) != "null"
			if !isPR && is.Title == title {
				return &issues[i], nil
			}
		}
		if len(issues) < 100 {
			return nil, nil
		}
	}
	return nil, nil
}

func (g *github) createIssue(owner, repo, title, body string, labels []string) (ghIssue, error) {
	b, _ := json.Marshal(map[string]any{"title": title, "body": body, "labels": labels})
	var is ghIssue
	err := g.do(http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/issues", g.api, owner, repo),
		"application/vnd.github+json", bytes.NewReader(b), "application/json", &is)
	return is, err
}

// repoOf reads OWNER/REPO, as GITHUB_REPOSITORY holds it.
func repoOf(s string) (owner, repo string, err error) {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", fmt.Errorf("%q is not OWNER/REPO", s)
	}
	return owner, repo, nil
}
