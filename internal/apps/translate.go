package apps

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	NamespacePrefix     = "app-"
	ManagedByLabel      = "app.kubernetes.io/managed-by"
	ManagedByValue      = "orchestrator"
	AppNameLabel        = "orchestrator.app/name"
	HostPathBaseDefault = "/var/lib/orchestrator/apps"
)

// Manifests is the result of translating one template.
type Manifests struct {
	Namespace string
	Objects   []*unstructured.Unstructured
	// Notes accumulates non-fatal translation notes (unsupported features, etc).
	Notes []string
}

// Translate produces ready-to-apply manifests from a template plus user env.
func Translate(ctx context.Context, t Template, envValues map[string]string, hostPathBase string) (*Manifests, error) {
	if hostPathBase == "" {
		hostPathBase = HostPathBaseDefault
	}
	slug := t.Slug()
	ns := NamespacePrefix + slug
	out := &Manifests{Namespace: ns}
	out.Objects = append(out.Objects, namespaceObject(ns, slug))

	switch t.Type {
	case 1:
		objs, notes := translateContainer(t, envValues, ns, slug, hostPathBase)
		out.Objects = append(out.Objects, objs...)
		out.Notes = append(out.Notes, notes...)
	case 2, 3:
		raw, err := fetchStackFile(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("fetch compose file: %w", err)
		}
		objs, notes, err := translateCompose(raw, envValues, ns, slug, hostPathBase)
		if err != nil {
			return nil, fmt.Errorf("translate compose: %w", err)
		}
		out.Objects = append(out.Objects, objs...)
		out.Notes = append(out.Notes, notes...)
	case 4:
		raw, err := fetchStackFile(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("fetch manifest: %w", err)
		}
		objs, err := splitYAMLDocuments(raw)
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
	default:
		return nil, fmt.Errorf("%w: type %d", ErrUnsupportedType, t.Type)
	}
	return out, nil
}

// fetchStackFile downloads the compose/manifest from the template's repository.
// Tries the main branch first then master.
func fetchStackFile(ctx context.Context, t Template) ([]byte, error) {
	if t.Repository == nil || t.Repository.URL == "" || t.Repository.Stackfile == "" {
		return nil, fmt.Errorf("template has no repository/stackfile")
	}
	for _, branch := range []string{"main", "master"} {
		url, err := rawGitHubURL(t.Repository.URL, t.Repository.Stackfile, branch)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == 404 {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, fmt.Errorf("could not fetch stackfile %q from %s", t.Repository.Stackfile, t.Repository.URL)
}

func rawGitHubURL(repoURL, file, branch string) (string, error) {
	repoURL = strings.TrimSuffix(strings.TrimSuffix(repoURL, "/"), ".git")
	if !strings.HasPrefix(repoURL, "https://github.com/") {
		return strings.TrimSuffix(repoURL, "/") + "/" + strings.TrimPrefix(file, "/"), nil
	}
	rest := strings.TrimPrefix(repoURL, "https://github.com/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("unexpected repo URL: %s", repoURL)
	}
	return "https://raw.githubusercontent.com/" + parts[0] + "/" + parts[1] + "/" + branch + "/" + strings.TrimPrefix(file, "/"), nil
}

// ---------------- Type 1: single container ----------------

type parsedPort struct {
	HostPort      int32
	ContainerPort int32
	Protocol      corev1.Protocol
}

var portRe = regexp.MustCompile(`^(?:(\d+):)?(\d+)(?:/(tcp|udp))?$`)

func parsePort(p string) (parsedPort, bool) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, ":")
	m := portRe.FindStringSubmatch(p)
	if m == nil {
		return parsedPort{}, false
	}
	out := parsedPort{Protocol: corev1.ProtocolTCP}
	if m[1] != "" {
		v, _ := strconv.Atoi(m[1])
		out.HostPort = int32(v)
	}
	if m[2] != "" {
		v, _ := strconv.Atoi(m[2])
		out.ContainerPort = int32(v)
	}
	if strings.EqualFold(m[3], "udp") {
		out.Protocol = corev1.ProtocolUDP
	}
	return out, true
}

func translateContainer(t Template, envValues map[string]string, ns, slug, hostPathBase string) ([]*unstructured.Unstructured, []string) {
	var notes []string
	containerName := slug
	if containerName == "" {
		containerName = "app"
	}

	envEntries := make([]corev1.EnvVar, 0, len(t.Env))
	for _, e := range t.Env {
		val := e.Default
		if v, ok := envValues[e.Name]; ok {
			val = v
		}
		envEntries = append(envEntries, corev1.EnvVar{Name: e.Name, Value: val})
	}

	var ports []corev1.ContainerPort
	for _, p := range t.Ports {
		if pp, ok := parsePort(p); ok && pp.ContainerPort > 0 {
			ports = append(ports, corev1.ContainerPort{
				ContainerPort: pp.ContainerPort,
				Protocol:      pp.Protocol,
			})
		} else if !ok {
			notes = append(notes, fmt.Sprintf("ignored port spec %q", p))
		}
	}

	var volMounts []corev1.VolumeMount
	var volumes []corev1.Volume
	for i, v := range t.Volumes {
		volName := fmt.Sprintf("vol-%d", i)
		volMounts = append(volMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: v.Container,
			ReadOnly:  v.Readonly,
		})
		volumes = append(volumes, makeVolume(volName, v, slug, hostPathBase))
	}

	container := corev1.Container{
		Name:         containerName,
		Image:        t.Image,
		Env:          envEntries,
		Ports:        ports,
		VolumeMounts: volMounts,
	}
	if t.Command != "" {
		container.Command = strings.Fields(t.Command)
	}
	if t.Privileged {
		priv := true
		container.SecurityContext = &corev1.SecurityContext{Privileged: &priv}
	}

	deploy := buildDeployment(ns, slug, container, volumes)
	objs := []*unstructured.Unstructured{toUnstructured(deploy)}

	if svc := buildService(ns, slug, t.Ports); svc != nil {
		objs = append(objs, toUnstructured(svc))
	}
	for _, o := range objs {
		applyManagedLabels(o, slug)
	}
	return objs, notes
}

func makeVolume(name string, v Volume, slug, hostPathBase string) corev1.Volume {
	if v.Bind != "" && strings.HasPrefix(v.Bind, "/") {
		hp := corev1.HostPathDirectoryOrCreate
		return corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: path.Join(hostPathBase, slug, sanitizePathSegment(v.Bind)),
					Type: &hp,
				},
			},
		}
	}
	// Anonymous or named volumes become emptyDir for v1.
	return corev1.Volume{
		Name:         name,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

var pathSegRe = regexp.MustCompile(`[^a-z0-9-]+`)

// chownInitContainer returns an init container that opens up the permissions
// of every host-path mount so non-root containers can write to them. Returns
// nil when there are no host-path mounts.
func chownInitContainer(mounts []corev1.VolumeMount) map[string]interface{} {
	if len(mounts) == 0 {
		return nil
	}
	mountList := make([]interface{}, 0, len(mounts))
	paths := make([]string, 0, len(mounts))
	for _, m := range mounts {
		mountList = append(mountList, map[string]interface{}{"name": m.Name, "mountPath": m.MountPath})
		paths = append(paths, m.MountPath)
	}
	cmd := "chmod -R 0777 " + strings.Join(paths, " ")
	return map[string]interface{}{
		"name":         "orchestrator-init-perms",
		"image":        "busybox:1.36",
		"command":      []interface{}{"sh", "-c", cmd},
		"volumeMounts": mountList,
		"securityContext": map[string]interface{}{
			"runAsUser":  int64(0),
			"runAsGroup": int64(0),
		},
	}
}

func sanitizePathSegment(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = strings.ToLower(p)
	p = pathSegRe.ReplaceAllString(p, "-")
	p = strings.Trim(p, "-")
	if p == "" {
		return "data"
	}
	return p
}

func buildDeployment(ns, slug string, container corev1.Container, volumes []corev1.Volume) *unstructured.Unstructured {
	labels := map[string]interface{}{
		"app":          slug,
		ManagedByLabel: ManagedByValue,
		AppNameLabel:   slug,
	}
	envList := make([]interface{}, 0, len(container.Env))
	for _, e := range container.Env {
		envList = append(envList, map[string]interface{}{"name": e.Name, "value": e.Value})
	}
	portList := make([]interface{}, 0, len(container.Ports))
	for _, p := range container.Ports {
		portList = append(portList, map[string]interface{}{
			"containerPort": int64(p.ContainerPort),
			"protocol":      string(p.Protocol),
		})
	}
	mountList := make([]interface{}, 0, len(container.VolumeMounts))
	for _, m := range container.VolumeMounts {
		entry := map[string]interface{}{"name": m.Name, "mountPath": m.MountPath}
		if m.ReadOnly {
			entry["readOnly"] = true
		}
		mountList = append(mountList, entry)
	}
	containerMap := map[string]interface{}{
		"name":         container.Name,
		"image":        container.Image,
		"env":          envList,
		"ports":        portList,
		"volumeMounts": mountList,
	}
	if len(container.Command) > 0 {
		cmd := make([]interface{}, 0, len(container.Command))
		for _, c := range container.Command {
			cmd = append(cmd, c)
		}
		containerMap["command"] = cmd
	}
	if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		containerMap["securityContext"] = map[string]interface{}{"privileged": true}
	}

	volList := make([]interface{}, 0, len(volumes))
	var hostPathMounts []corev1.VolumeMount
	for i, v := range volumes {
		entry := map[string]interface{}{"name": v.Name}
		switch {
		case v.HostPath != nil:
			t := ""
			if v.HostPath.Type != nil {
				t = string(*v.HostPath.Type)
			}
			hp := map[string]interface{}{"path": v.HostPath.Path}
			if t != "" {
				hp["type"] = t
			}
			entry["hostPath"] = hp
			if i < len(container.VolumeMounts) {
				hostPathMounts = append(hostPathMounts, container.VolumeMounts[i])
			}
		default:
			entry["emptyDir"] = map[string]interface{}{}
		}
		volList = append(volList, entry)
	}

	podSpec := map[string]interface{}{
		"containers": []interface{}{containerMap},
		"volumes":    volList,
		// Kubernetes auto-injects SVC_PORT env vars for every Service in the
		// namespace, e.g. N8N_PORT=tcp://10.x.x.x:5678. That collides with the
		// app's own configuration, so we disable the legacy service links.
		"enableServiceLinks": false,
	}
	if init := chownInitContainer(hostPathMounts); init != nil {
		podSpec["initContainers"] = []interface{}{init}
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]interface{}{
			"name":      slug,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": slug}},
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": labels},
				"spec":     podSpec,
			},
		},
	}}
	return obj
}

func buildService(ns, slug string, ports []string) *unstructured.Unstructured {
	var svcPorts []interface{}
	seen := map[string]struct{}{}
	for _, p := range ports {
		pp, ok := parsePort(p)
		if !ok || pp.ContainerPort == 0 {
			continue
		}
		key := fmt.Sprintf("%d/%s", pp.ContainerPort, pp.Protocol)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		svcPorts = append(svcPorts, map[string]interface{}{
			"name":       fmt.Sprintf("p%d-%s", pp.ContainerPort, strings.ToLower(string(pp.Protocol))),
			"port":       int64(pp.ContainerPort),
			"targetPort": int64(pp.ContainerPort),
			"protocol":   string(pp.Protocol),
		})
	}
	if len(svcPorts) == 0 {
		return nil
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name":      slug,
			"namespace": ns,
		},
		"spec": map[string]interface{}{
			"type":     "ClusterIP",
			"selector": map[string]interface{}{"app": slug},
			"ports":    svcPorts,
		},
	}}
}

func namespaceObject(ns, slug string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": ns,
			"labels": map[string]interface{}{
				ManagedByLabel: ManagedByValue,
				AppNameLabel:   slug,
			},
		},
	}}
}

func toUnstructured(in interface{}) *unstructured.Unstructured {
	switch v := in.(type) {
	case *unstructured.Unstructured:
		return v
	case runtime.Object:
		// Round-trip via JSON to avoid scheme registration.
		data, err := sigsyaml.Marshal(v)
		if err != nil {
			return &unstructured.Unstructured{}
		}
		out, err := yamlToUnstructured(data)
		if err != nil {
			return &unstructured.Unstructured{}
		}
		return out
	}
	return &unstructured.Unstructured{}
}

func applyManagedLabels(o *unstructured.Unstructured, slug string) {
	labels := o.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ManagedByLabel] = ManagedByValue
	labels[AppNameLabel] = slug
	o.SetLabels(labels)
}

func shouldNamespace(o *unstructured.Unstructured) bool {
	if o.GetNamespace() != "" {
		return false
	}
	switch o.GetKind() {
	case "Namespace", "ClusterRole", "ClusterRoleBinding",
		"PersistentVolume", "StorageClass", "CustomResourceDefinition",
		"PriorityClass", "Node", "ValidatingWebhookConfiguration",
		"MutatingWebhookConfiguration", "APIService":
		return false
	}
	return true
}

// ---------------- YAML helpers ----------------

func splitYAMLDocuments(data []byte) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	docs := splitYAMLDocs(data)
	for _, d := range docs {
		d = []byte(strings.TrimSpace(string(d)))
		if len(d) == 0 {
			continue
		}
		obj, err := yamlToUnstructured(d)
		if err != nil {
			return nil, fmt.Errorf("decode manifest doc: %w", err)
		}
		if obj == nil || obj.GetKind() == "" {
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

func splitYAMLDocs(data []byte) [][]byte {
	// Split on lines that are exactly "---" (with optional trailing whitespace).
	var docs [][]byte
	lines := strings.Split(string(data), "\n")
	current := []string{}
	for _, l := range lines {
		if strings.TrimSpace(l) == "---" {
			if len(current) > 0 {
				docs = append(docs, []byte(strings.Join(current, "\n")))
				current = current[:0]
			}
			continue
		}
		current = append(current, l)
	}
	if len(current) > 0 {
		docs = append(docs, []byte(strings.Join(current, "\n")))
	}
	return docs
}

func yamlToUnstructured(data []byte) (*unstructured.Unstructured, error) {
	jsonData, err := sigsyaml.YAMLToJSON(data)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(jsonData)) == "null" {
		return nil, nil
	}
	obj := &unstructured.Unstructured{}
	if _, _, err := unstructured.UnstructuredJSONScheme.Decode(jsonData, nil, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// ---------------- Type 2/3: docker-compose ----------------

type composeFile struct {
	Version  string                    `yaml:"version" json:"version"`
	Services map[string]composeService `yaml:"services" json:"services"`
}

type composeService struct {
	Image       string      `json:"image"`
	Command     interface{} `json:"command"`
	Ports       []string    `json:"ports"`
	Volumes     []string    `json:"volumes"`
	Environment interface{} `json:"environment"`
	Restart     string      `json:"restart"`
	Privileged  bool        `json:"privileged"`
}

func translateCompose(raw []byte, envValues map[string]string, ns, slug, hostPathBase string) ([]*unstructured.Unstructured, []string, error) {
	jsonData, err := sigsyaml.YAMLToJSON(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse compose yaml: %w", err)
	}
	var f composeFile
	if err := sigsyaml.Unmarshal(jsonData, &f); err != nil {
		return nil, nil, fmt.Errorf("decode compose: %w", err)
	}
	if len(f.Services) == 0 {
		return nil, nil, fmt.Errorf("compose has no services")
	}
	var notes []string
	var objs []*unstructured.Unstructured

	names := make([]string, 0, len(f.Services))
	for k := range f.Services {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		svc := f.Services[name]
		dnsName := SanitizeSlug(name)
		if dnsName == "" {
			dnsName = "svc"
		}

		envEntries := composeEnv(svc.Environment, envValues)
		ports := composePorts(svc.Ports)
		volMounts, volumes, vNotes := composeVolumes(svc.Volumes, dnsName, slug, hostPathBase)
		notes = append(notes, vNotes...)

		container := corev1.Container{
			Name:  dnsName,
			Image: svc.Image,
			Env:   envEntries,
		}
		for _, p := range ports {
			container.Ports = append(container.Ports, corev1.ContainerPort{
				ContainerPort: p.ContainerPort, Protocol: p.Protocol,
			})
		}
		container.VolumeMounts = volMounts
		switch c := svc.Command.(type) {
		case string:
			if c != "" {
				container.Command = strings.Fields(c)
			}
		case []interface{}:
			for _, cc := range c {
				container.Command = append(container.Command, fmt.Sprintf("%v", cc))
			}
		}
		if svc.Privileged {
			priv := true
			container.SecurityContext = &corev1.SecurityContext{Privileged: &priv}
		}

		deploy := buildDeployment(ns, dnsName, container, volumes)
		// Use service name as the workload selector instead of slug.
		setSelector(deploy, dnsName)
		objs = append(objs, deploy)

		var rawPorts []string
		for _, p := range ports {
			rawPorts = append(rawPorts, fmt.Sprintf("%d/%s", p.ContainerPort, strings.ToLower(string(p.Protocol))))
		}
		if s := buildService(ns, dnsName, rawPorts); s != nil {
			setSelector(s, dnsName)
			objs = append(objs, s)
		}
	}
	for _, o := range objs {
		applyManagedLabels(o, slug)
	}
	return objs, notes, nil
}

func setSelector(o *unstructured.Unstructured, app string) {
	if o.GetKind() == "Deployment" {
		_ = unstructuredSetMap(o, []string{"spec", "selector", "matchLabels"}, map[string]interface{}{"app": app})
		_ = unstructuredSetMap(o, []string{"spec", "template", "metadata", "labels"}, map[string]interface{}{"app": app, ManagedByLabel: ManagedByValue, AppNameLabel: getLabelValue(o, AppNameLabel)})
		_ = unstructuredSetMap(o, []string{"metadata", "labels"}, map[string]interface{}{"app": app, ManagedByLabel: ManagedByValue, AppNameLabel: getLabelValue(o, AppNameLabel)})
	}
	if o.GetKind() == "Service" {
		_ = unstructuredSetMap(o, []string{"spec", "selector"}, map[string]interface{}{"app": app})
	}
}

func unstructuredSetMap(o *unstructured.Unstructured, keys []string, val map[string]interface{}) error {
	cur := o.Object
	for i, k := range keys {
		if i == len(keys)-1 {
			cur[k] = val
			return nil
		}
		next, _ := cur[k].(map[string]interface{})
		if next == nil {
			next = map[string]interface{}{}
			cur[k] = next
		}
		cur = next
	}
	return nil
}

func getLabelValue(o *unstructured.Unstructured, key string) string {
	l := o.GetLabels()
	if l == nil {
		return ""
	}
	return l[key]
}

func composeEnv(env interface{}, overrides map[string]string) []corev1.EnvVar {
	var out []corev1.EnvVar
	switch e := env.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := fmt.Sprintf("%v", e[k])
			if v, ok := overrides[k]; ok {
				val = v
			}
			out = append(out, corev1.EnvVar{Name: k, Value: val})
		}
	case []interface{}:
		for _, item := range e {
			s, ok := item.(string)
			if !ok {
				continue
			}
			parts := strings.SplitN(s, "=", 2)
			name := parts[0]
			val := ""
			if len(parts) == 2 {
				val = parts[1]
			}
			if v, ok := overrides[name]; ok {
				val = v
			}
			out = append(out, corev1.EnvVar{Name: name, Value: val})
		}
	}
	return out
}

func composePorts(ports []string) []parsedPort {
	var out []parsedPort
	for _, p := range ports {
		if pp, ok := parsePort(p); ok && pp.ContainerPort > 0 {
			out = append(out, pp)
		}
	}
	return out
}

func composeVolumes(vols []string, svcName, slug, hostPathBase string) ([]corev1.VolumeMount, []corev1.Volume, []string) {
	var notes []string
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	for i, v := range vols {
		parts := strings.SplitN(v, ":", 3)
		var bind, container string
		ro := false
		switch len(parts) {
		case 1:
			container = parts[0]
		case 2:
			bind, container = parts[0], parts[1]
		case 3:
			bind, container = parts[0], parts[1]
			if strings.Contains(parts[2], "ro") {
				ro = true
			}
		}
		if container == "" {
			notes = append(notes, fmt.Sprintf("ignored volume spec %q", v))
			continue
		}
		name := fmt.Sprintf("%s-vol-%d", svcName, i)
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: container, ReadOnly: ro})
		volumes = append(volumes, makeVolume(name, Volume{Bind: bind, Container: container, Readonly: ro}, slug, hostPathBase))
	}
	return mounts, volumes, notes
}

// ---------------- Logo decoding ----------------

// DecodeDataURI returns the bytes encoded in a data: URI, or nil if it isn't one.
func DecodeDataURI(s string) ([]byte, string) {
	if !strings.HasPrefix(s, "data:") {
		return nil, ""
	}
	rest := strings.TrimPrefix(s, "data:")
	idx := strings.Index(rest, ",")
	if idx < 0 {
		return nil, ""
	}
	header := rest[:idx]
	payload := rest[idx+1:]
	mime := strings.SplitN(header, ";", 2)[0]
	if strings.HasSuffix(header, ";base64") {
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, ""
		}
		return data, mime
	}
	return []byte(payload), mime
}

// LabelMap returns a map from a slice of labels.
func LabelMap(labels []Label) map[string]string {
	out := map[string]string{}
	for _, l := range labels {
		out[l.Name] = l.Value
	}
	return out
}

// keep metav1 import in use in case we later marshal typed objects
var _ = metav1.ObjectMeta{}
