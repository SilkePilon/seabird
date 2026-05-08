// Package apps provides the Portainer-template catalog client and a
// translator that turns those templates into Kubernetes manifests.
package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	CatalogURL = "https://raw.githubusercontent.com/Lissy93/portainer-templates/main/templates.json"
	CacheTTL   = 24 * time.Hour
)

type Repository struct {
	URL       string `json:"url"`
	Stackfile string `json:"stackfile"`
}

type EnvSelectOpt struct {
	Text    string `json:"text"`
	Value   string `json:"value"`
	Default bool   `json:"default"`
}

type EnvVar struct {
	Name        string         `json:"name"`
	Label       string         `json:"label"`
	Default     string         `json:"default"`
	Description string         `json:"description"`
	Preset      bool           `json:"preset"`
	Select      []EnvSelectOpt `json:"select"`
}

type Volume struct {
	Container string `json:"container"`
	Bind      string `json:"bind"`
	Readonly  bool   `json:"readonly"`
}

type Label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Template mirrors one entry in the Portainer template JSON. We accept extra
// fields silently.
type Template struct {
	ID            int         `json:"id"`
	Type          int         `json:"type"`
	Title         string      `json:"title"`
	Name          string      `json:"name"`
	Description   string      `json:"description"`
	Note          string      `json:"note"`
	Logo          string      `json:"logo"`
	Categories    []string    `json:"categories"`
	Platform      string      `json:"platform"`
	Image         string      `json:"image"`
	Maintainer    string      `json:"maintainer"`
	Ports         []string    `json:"ports"`
	Volumes       []Volume    `json:"volumes"`
	Env           []EnvVar    `json:"env"`
	Labels        []Label     `json:"labels"`
	Command       string      `json:"command"`
	RestartPolicy string      `json:"restart_policy"`
	Privileged    bool        `json:"privileged"`
	Repository    *Repository `json:"repository"`
}

type catalogFile struct {
	Version   string     `json:"version"`
	Templates []Template `json:"templates"`
}

type Catalog struct {
	Templates []Template
	FetchedAt time.Time
}

// ErrUnsupportedType is returned by Translate for unknown template types.
var ErrUnsupportedType = errors.New("template type not supported")

// LoadCatalog returns the catalog, using a 24h on-disk cache unless force is true.
func LoadCatalog(ctx context.Context, force bool) (*Catalog, error) {
	p := cachePath()
	if !force {
		if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) < CacheTTL {
			if data, err := os.ReadFile(p); err == nil {
				if c, err := parseCatalog(data, info.ModTime()); err == nil {
					return c, nil
				}
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CatalogURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("catalog: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, data, 0o644)
	return parseCatalog(data, time.Now())
}

func cachePath() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "orchestrator", "portainer-templates.json")
}

func parseCatalog(data []byte, fetchedAt time.Time) (*Catalog, error) {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	sort.SliceStable(f.Templates, func(i, j int) bool {
		return strings.ToLower(f.Templates[i].DisplayName()) < strings.ToLower(f.Templates[j].DisplayName())
	})
	return &Catalog{Templates: f.Templates, FetchedAt: fetchedAt}, nil
}

// Categories returns a deduplicated, sorted list of all categories present.
func (c *Catalog) Categories() []string {
	seen := map[string]struct{}{}
	for _, t := range c.Templates {
		for _, cat := range t.Categories {
			cat = strings.TrimSpace(cat)
			if cat == "" {
				continue
			}
			seen[cat] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (t Template) DisplayName() string {
	if t.Title != "" {
		return t.Title
	}
	return t.Name
}

func (t Template) Slug() string {
	s := t.Name
	if s == "" {
		s = t.Title
	}
	return SanitizeSlug(s)
}

func (t Template) TypeLabel() string {
	switch t.Type {
	case 1:
		return "Container"
	case 2:
		return "Swarm Stack"
	case 3:
		return "Compose Stack"
	case 4:
		return "Kubernetes"
	}
	return "Unknown"
}

func (t Template) CategoriesText() string {
	return strings.Join(t.Categories, " · ")
}

// SanitizeSlug returns a DNS-1123-safe slug derived from s.
func SanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "app"
	}
	if len(out) > 50 {
		out = strings.TrimRight(out[:50], "-")
	}
	return out
}
