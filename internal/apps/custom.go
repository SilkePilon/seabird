package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"
)

// CustomKind selects how a raw user-supplied app definition is translated
// into Kubernetes manifests.
type CustomKind string

const (
	// CustomKindCompose treats the input as a docker-compose YAML/JSON file.
	CustomKindCompose CustomKind = "compose"
	// CustomKindManifest treats the input as one or more raw Kubernetes YAML
	// documents that should be applied verbatim (with Apps-managed labels and
	// namespace assignment).
	CustomKindManifest CustomKind = "manifest"
	// CustomKindContainer treats the input as a Portainer-style template JSON
	// describing a single container.
	CustomKindContainer CustomKind = "container"
)

// TranslateCustom converts a raw user-pasted definition into a Manifests
// bundle ready for Apply. The slug is sanitised; if empty, an error is
// returned.
func TranslateCustom(ctx context.Context, kind CustomKind, raw []byte, slug string, envValues map[string]string, hostPathBase string) (*Manifests, error) {
	if hostPathBase == "" {
		hostPathBase = HostPathBaseDefault
	}
	slug = SanitizeSlug(slug)
	if slug == "" {
		return nil, fmt.Errorf("a name is required")
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("input is empty")
	}
	if envValues == nil {
		envValues = map[string]string{}
	}

	ns := NamespacePrefix + slug
	out := &Manifests{Namespace: ns}
	out.Objects = append(out.Objects, namespaceObject(ns, slug))

	switch kind {
	case CustomKindCompose:
		yml, err := jsonOrYAMLToYAML(raw)
		if err != nil {
			return nil, fmt.Errorf("parse compose: %w", err)
		}
		objs, notes, err := translateCompose(yml, envValues, ns, slug, hostPathBase)
		if err != nil {
			return nil, fmt.Errorf("translate compose: %w", err)
		}
		out.Objects = append(out.Objects, objs...)
		out.Notes = append(out.Notes, notes...)

	case CustomKindManifest:
		yml, err := jsonOrYAMLToYAML(raw)
		if err != nil {
			return nil, fmt.Errorf("parse manifest: %w", err)
		}
		objs, err := splitYAMLDocuments(yml)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			applyManagedLabels(o, slug)
			if shouldNamespace(o) {
				o.SetNamespace(ns)
			}
		}
		out.Objects = append(out.Objects, objs...)

	case CustomKindContainer:
		var t Template
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("parse container template JSON: %w", err)
		}
		if t.Image == "" {
			return nil, fmt.Errorf("container template requires an \"image\"")
		}
		// Force single-container type and a fresh title so Slug() resolves
		// to the user-provided slug (translateContainer doesn't read t.Title).
		t.Type = 1
		objs, notes := translateContainer(t, envValues, ns, slug, hostPathBase)
		out.Objects = append(out.Objects, objs...)
		out.Notes = append(out.Notes, notes...)

	default:
		return nil, fmt.Errorf("unknown custom kind %q", kind)
	}

	return out, nil
}

// jsonOrYAMLToYAML accepts either YAML or JSON input and returns canonical
// YAML bytes suitable for the existing YAML-based translators.
func jsonOrYAMLToYAML(raw []byte) ([]byte, error) {
	trimmed := strings.TrimLeft(string(raw), " \t\r\n")
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return sigsyaml.JSONToYAML(raw)
	}
	return raw, nil
}
