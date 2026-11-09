package trackers

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
)

// ImageFetcher is a tracker whose issues point at pictures only its token may read (GitLab's /uploads/…): the board
// asks for them through Mako, since the browser has no session there.
type ImageFetcher interface {
	// Image returns the bytes of the picture at path, relative to the project's issues ("/uploads/<secret>/<file>").
	Image(ctx context.Context, project, path string) ([]byte, error)
}

// uploadRef: a GitLab upload, as an issue's text names it. Nothing else is fetched with Mako's token.
var uploadRef = regexp.MustCompile(`^/uploads/([0-9a-fA-F]{16,64})/([^/\\?#]+)$`)

// Image downloads a picture the project's issues hold, through the API (the web path takes no token).
func (g *GitLab) Image(ctx context.Context, projectPath, path string) ([]byte, error) {
	m := uploadRef.FindStringSubmatch(path)
	if m == nil || projectPath == "" {
		return nil, fmt.Errorf("not a GitLab upload: %q", path)
	}
	var data string
	if err := g.do(ctx, "GET", project(projectPath)+"/uploads/"+m[1]+"/"+url.PathEscape(m[2]), nil, &data); err != nil {
		return nil, err
	}
	return []byte(data), nil
}
