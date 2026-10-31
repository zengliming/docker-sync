package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// tagPageSize is the page size we ask the registry for. Docker Hub caps it
	// at 1000; smaller registries return everything at once.
	tagPageSize = 1000
	// maxTagPages bounds the pagination walk so a broken Link header cannot
	// spin forever.
	maxTagPages = 20
	// maxTagList caps how many tags are kept in memory.
	maxTagList = 20000
)

// ListTags returns the tags the upstream publishes for the repository, walking
// the Link header pagination used by Docker Hub and friends. Tags keep the
// upstream order and duplicates are dropped.
func (c *Client) ListTags(ctx context.Context, ref Ref) ([]string, error) {
	if ref.Repository == "" {
		return nil, fmt.Errorf("registry: empty repository in %q", ref.String())
	}
	page := c.url(ref, "/v2/"+ref.Repository+"/tags/list?n="+strconv.Itoa(tagPageSize))
	seen := make(map[string]bool)
	var tags []string
	for pages := 0; page != "" && pages < maxTagPages; pages++ {
		resp, err := c.do(ctx, ref, page, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return nil, fmt.Errorf("registry: repository %s not found in %s", ref.Repository, ref.Endpoint())
			}
			return nil, fmt.Errorf("registry: list tags of %s: %s: %s", ref.Repository, resp.Status, snippet(body))
		}
		next := c.nextPage(resp, ref)
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		drainAndClose(resp)
		if readErr != nil {
			return nil, fmt.Errorf("registry: read tags of %s: %w", ref.Repository, readErr)
		}
		var payload struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("registry: decode tags of %s: %w", ref.Repository, err)
		}
		for _, tag := range payload.Tags {
			if tag == "" || seen[tag] {
				continue
			}
			seen[tag] = true
			tags = append(tags, tag)
			if len(tags) >= maxTagList {
				return tags, nil
			}
		}
		page = next
	}
	return tags, nil
}

// nextPage extracts the rel="next" URL from a Link header. Only links that stay
// on the registry endpoint we authenticated against are followed, so a hostile
// Link header cannot make us send a token to another host.
func (c *Client) nextPage(resp *http.Response, ref Ref) string {
	endpoint := ref.Endpoint()
	for _, value := range resp.Header.Values("Link") {
		for _, part := range strings.Split(value, ",") {
			segments := strings.Split(part, ";")
			if len(segments) < 2 {
				continue
			}
			target := strings.TrimSpace(segments[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				continue
			}
			isNext := false
			for _, attr := range segments[1:] {
				attr = strings.TrimSpace(attr)
				if strings.EqualFold(attr, `rel="next"`) || strings.EqualFold(attr, "rel=next") {
					isNext = true
				}
			}
			if !isNext {
				continue
			}
			link := strings.TrimSuffix(strings.TrimPrefix(target, "<"), ">")
			if strings.HasPrefix(link, "/") {
				return c.url(ref, link)
			}
			parsed, err := url.Parse(link)
			if err != nil || parsed.Host == "" {
				continue
			}
			if !strings.EqualFold(parsed.Host, endpoint) {
				continue
			}
			return parsed.String()
		}
	}
	return ""
}
