package extension

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/SilkePilon/Orchestrator/api"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

type PortForwarder struct {
	*api.Cluster
	forwarders       map[types.NamespacedName]*portforward.PortForwarder
	deploymentByPod  map[types.NamespacedName]types.NamespacedName
	podsByDeployment map[types.NamespacedName]map[types.NamespacedName]struct{}
}

// portForwarders is a small process-wide registry that lets other extensions
// (e.g. Meta, which renders the universal Name column) discover the active
// PortForwarder for a given cluster without taking a hard dependency on the
// Core extension's internals.
var portForwarders = map[*api.Cluster]*PortForwarder{}

func NewPortForwarder(cluster *api.Cluster) *PortForwarder {
	p := &PortForwarder{
		Cluster:          cluster,
		forwarders:       map[types.NamespacedName]*portforward.PortForwarder{},
		deploymentByPod:  map[types.NamespacedName]types.NamespacedName{},
		podsByDeployment: map[types.NamespacedName]map[types.NamespacedName]struct{}{},
	}
	portForwarders[cluster] = p
	return p
}

// PortForwarderFor returns the active PortForwarder for the given cluster, or
// nil if none has been registered (no Core extension yet).
func PortForwarderFor(cluster *api.Cluster) *PortForwarder {
	return portForwarders[cluster]
}

func (p *PortForwarder) New(ctx context.Context, name types.NamespacedName, ports []string) error {
	readyChan := make(chan struct{}, 1)
	errChan := make(chan error, 1)

	url := p.Clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(name.Namespace).Name(name.Name).SubResource("portforward").URL()
	transport, upgrader, err := spdy.RoundTripperFor(p.Config)
	if err != nil {
		return err
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	tunnelingDialer, err := portforward.NewSPDYOverWebsocketDialer(url, p.Config)
	if err != nil {
		return err
	}
	dialer = portforward.NewFallbackDialer(tunnelingDialer, dialer, httpstream.IsUpgradeFailure)

	forwarder, err := portforward.NewOnAddresses(dialer, []string{"localhost"}, ports, ctx.Done(), readyChan, nil, os.Stderr)
	if err != nil {
		return err
	}
	p.forwarders[name] = forwarder

	// Best-effort: resolve the pod's owning Deployment so the indicator on the
	// Deployments list view also lights up. Errors are non-fatal.
	if dep, ok := p.resolveDeployment(ctx, name); ok {
		p.deploymentByPod[name] = dep
		set := p.podsByDeployment[dep]
		if set == nil {
			set = map[types.NamespacedName]struct{}{}
			p.podsByDeployment[dep] = set
		}
		set[name] = struct{}{}
	}

	go func() {
		if err := forwarder.ForwardPorts(); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-readyChan:
		return nil
	case err := <-errChan:
		return err
	case <-time.After(5 * time.Second):
		return errors.New("timeout")
	}
}

func (p *PortForwarder) GetPorts(name types.NamespacedName) ([]portforward.ForwardedPort, error) {
	if forwarder := p.forwarders[name]; forwarder != nil {
		return forwarder.GetPorts()
	} else {
		return nil, errors.New("not found")
	}
}

func (p *PortForwarder) Close(name types.NamespacedName) error {
	if forwarder := p.forwarders[name]; forwarder != nil {
		forwarder.Close()
		delete(p.forwarders, name)
		if dep, ok := p.deploymentByPod[name]; ok {
			delete(p.deploymentByPod, name)
			if set := p.podsByDeployment[dep]; set != nil {
				delete(set, name)
				if len(set) == 0 {
					delete(p.podsByDeployment, dep)
				}
			}
		}
		return nil
	} else {
		return errors.New("not found")
	}
}

// LocalPortForPod returns the first allocated local port for an active forward
// against the named pod, if any.
func (p *PortForwarder) LocalPortForPod(name types.NamespacedName) (uint16, bool) {
	fwd := p.forwarders[name]
	if fwd == nil {
		return 0, false
	}
	ports, err := fwd.GetPorts()
	if err != nil || len(ports) == 0 {
		return 0, false
	}
	return ports[0].Local, true
}

// LocalPortForDeployment returns the first allocated local port for any
// active forward against a pod owned by the named deployment.
func (p *PortForwarder) LocalPortForDeployment(name types.NamespacedName) (uint16, bool) {
	for pod := range p.podsByDeployment[name] {
		if port, ok := p.LocalPortForPod(pod); ok {
			return port, true
		}
	}
	return 0, false
}

// resolveDeployment walks pod.OwnerReferences -> ReplicaSet -> Deployment.
// Returns the deployment NamespacedName when it can be resolved.
func (p *PortForwarder) resolveDeployment(ctx context.Context, podName types.NamespacedName) (types.NamespacedName, bool) {
	var pod corev1.Pod
	if err := p.Cluster.Get(ctx, podName, &pod); err != nil {
		return types.NamespacedName{}, false
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind != "ReplicaSet" {
			continue
		}
		var rs appsv1.ReplicaSet
		if err := p.Cluster.Get(ctx, types.NamespacedName{Name: owner.Name, Namespace: pod.Namespace}, &rs); err != nil {
			continue
		}
		for _, rsOwner := range rs.OwnerReferences {
			if rsOwner.Kind == "Deployment" {
				return types.NamespacedName{Name: rsOwner.Name, Namespace: pod.Namespace}, true
			}
		}
	}
	return types.NamespacedName{}, false
}

func (p *PortForwarder) UpdateButton(ctx context.Context, btn *gtk.Button, name types.NamespacedName, ports []string) {
	var handle glib.SignalHandle
	if fwd, err := p.GetPorts(name); err != nil {
		btn.SetIconName("vertical-arrows-long-symbolic")
		btn.SetTooltipText("Forward port to localhost")
		btn.AddCSSClass("flat")
		handle = btn.ConnectClicked(func() {
			if err := p.New(ctx, name, ports); err != nil {
				widget.ShowErrorDialog(ctx, "Port forward error", err)
			} else {
				p.UpdateButton(ctx, btn, name, ports)
			}
			btn.HandlerDisconnect(handle)
		})
	} else {
		box := gtk.NewBox(gtk.OrientationHorizontal, 2)
		icon := gtk.NewImageFromIconName("cross-small-symbolic")
		icon.AddCSSClass("error")
		box.Append(icon)
		box.Append(gtk.NewLabel(fmt.Sprintf("%d", fwd[0].Local)))
		btn.SetChild(box)
		btn.RemoveCSSClass("flat")
		btn.SetTooltipText("Close forwarding port")
		handle = btn.ConnectClicked(func() {
			p.Close(name)
			p.UpdateButton(ctx, btn, name, ports)
			btn.HandlerDisconnect(handle)
		})
	}

}
