package gce

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/charliek/shed/internal/remote/provider"
)

// Image is a boot image as the API reports it.
type Image struct {
	// Name is the image's name. Path is "projects/<p>/global/images/<name>".
	Name string
	Path string
	// Family is the image family, or "".
	Family string
	// Status is READY, PENDING, FAILED or DELETING.
	Status string
	// Deprecated is the deprecation state (DEPRECATED, OBSOLETE, DELETED),
	// or "" for a current image.
	Deprecated string
	// Arch is provider.ArchAMD64 or provider.ArchARM64 when the image says.
	Arch string
	// DiskSizeGB is the smallest boot disk the image fits.
	DiskSizeGB int
	// Labels are the image's labels.
	Labels map[string]string
}

type gceImage struct {
	Name         string            `json:"name"`
	SelfLink     string            `json:"selfLink"`
	Family       string            `json:"family"`
	Status       string            `json:"status"`
	Architecture string            `json:"architecture"`
	DiskSizeGb   int64String       `json:"diskSizeGb"`
	Labels       map[string]string `json:"labels"`
	Deprecated   *struct {
		State string `json:"state"`
	} `json:"deprecated"`
}

// imageRefRegex matches "projects/<p>/global/images/<name>" and
// "projects/<p>/global/images/family/<family>".
var imageRefRegex = regexp.MustCompile(`^projects/([a-z][-a-z0-9.:]*)/global/images/(family/)?([a-z]([-a-z0-9]*[a-z0-9])?)$`)

// imageNameRegex is a bare image or family name.
var imageNameRegex = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// LookupImage reads an image. ref is one of:
//
//   - "<name>": an image in the client's project;
//   - "family/<family>": the newest image of a family in the client's project;
//   - "projects/<p>/global/images/<name>": an image in another project;
//   - "projects/<p>/global/images/family/<family>": the newest image of a
//     family in another project (for example ubuntu-os-cloud).
//
// ErrNotFound means there is no such image or family.
func (c *Client) LookupImage(ctx context.Context, ref string) (Image, error) {
	project, name, family, err := c.parseImageRef(ref)
	if err != nil {
		return Image{}, err
	}
	path := "projects/" + project + "/global/images/"
	if family {
		path += "family/"
	}
	path += name
	var g gceImage
	if err := c.call(ctx, request{op: "resolve_image", method: http.MethodGet, path: path}, &g); err != nil {
		return Image{}, asProviderError("resolve_image", err)
	}
	img := Image{
		Name:       g.Name,
		Path:       "projects/" + project + "/global/images/" + g.Name,
		Family:     g.Family,
		Status:     g.Status,
		Arch:       archFromDisk(g.Architecture),
		DiskSizeGB: int(g.DiskSizeGb),
		Labels:     g.Labels,
	}
	if g.Deprecated != nil {
		img.Deprecated = g.Deprecated.State
	}
	return img, nil
}

func (c *Client) parseImageRef(ref string) (project, name string, family bool, err error) {
	if m := imageRefRegex.FindStringSubmatch(ref); m != nil {
		return m[1], m[3], m[2] != "", nil
	}
	if fam, ok := strings.CutPrefix(ref, provider.ImagePrefixFamily); ok {
		if imageNameRegex.MatchString(fam) {
			return c.cfg.Project, fam, true, nil
		}
	} else if imageNameRegex.MatchString(ref) {
		return c.cfg.Project, ref, false, nil
	}
	return "", "", false, provider.Errorf(provider.ErrInvalid, "resolve_image", "image %q is not a name, family/<name> or projects/<p>/global/images/... path", ref)
}

// ResolveImage implements provider.ImageResolver. An image in the client's
// project resolves to its bare name; one in another project to its
// "projects/..." path, which Create also accepts. An image that is not READY,
// or is OBSOLETE or DELETED, is refused with ErrNotFound.
func (c *Client) ResolveImage(ctx context.Context, image string) (string, error) {
	img, err := c.LookupImage(ctx, image)
	if err != nil {
		return "", err
	}
	if img.Status != "" && img.Status != "READY" {
		return "", provider.Errorf(provider.ErrNotFound, "resolve_image", "image %s is %s, not READY", img.Name, img.Status)
	}
	switch img.Deprecated {
	case "OBSOLETE", "DELETED":
		return "", provider.Errorf(provider.ErrNotFound, "resolve_image", "image %s is %s", img.Name, img.Deprecated)
	}
	if strings.HasPrefix(img.Path, c.projectPath()) {
		return img.Name, nil
	}
	return img.Path, nil
}
