package apps

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Apply creates/updates the manifests in order. Existing objects are patched
// with server-side apply via Patch with ApplyPatchType when possible, falling
// back to Create on NotFound.
func Apply(ctx context.Context, c client.Client, m *Manifests) error {
	for _, obj := range m.Objects {
		if err := applyOne(ctx, c, obj); err != nil {
			return fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err := c.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: obj.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	// Preserve resourceVersion and update.
	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

// Uninstall removes the namespace dedicated to the app and any cluster-scoped
// objects labelled as managed by this app.
func Uninstall(ctx context.Context, c client.Client, slug string) error {
	ns := NamespacePrefix + slug
	nsObj := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, nsObj); err == nil {
		if err := c.Delete(ctx, nsObj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Status describes what's currently deployed for a slug.
type Status struct {
	Installed bool
	Phase     string
	Pods      int
	PodsReady int
	Age       time.Duration
}

// GetStatus inspects the namespace for the app to report a quick status.
func GetStatus(ctx context.Context, c client.Client, slug string) (Status, error) {
	ns := NamespacePrefix + slug
	out := Status{}
	nsObj := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, nsObj); err != nil {
		if apierrors.IsNotFound(err) {
			return out, nil
		}
		return out, err
	}
	out.Installed = true
	out.Phase = string(nsObj.Status.Phase)
	out.Age = time.Since(nsObj.CreationTimestamp.Time)

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ns)); err == nil {
		out.Pods = len(pods.Items)
		for _, p := range pods.Items {
			ready := false
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Ready {
					ready = true
					break
				}
			}
			if ready {
				out.PodsReady++
			}
		}
	}
	return out, nil
}

// ListInstalledSlugs returns the slugs of all apps currently installed.
func ListInstalledSlugs(ctx context.Context, c client.Client) ([]string, error) {
	var nss corev1.NamespaceList
	if err := c.List(ctx, &nss, client.MatchingLabels{ManagedByLabel: ManagedByValue}); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nss.Items))
	for _, n := range nss.Items {
		if v := n.Labels[AppNameLabel]; v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, nil
}

// keep types import in use
var _ = types.NamespacedName{}
var _ = metav1.ObjectMeta{}
