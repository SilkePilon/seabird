package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

type HealthOverviewView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	page    *adw.PreferencesPage
	refresh *gtk.Button
	status  *gtk.Label

	groups []*adw.PreferencesGroup
}

type clusterHealthOverview struct {
	CollectedAt         time.Time
	NodesTotal          int
	NodesReady          int
	PodsTotal           int
	PodsRunning         int
	PodsPending         int
	PodsFailed          int
	PodWarnings         []healthFinding
	TotalRestarts       int32
	DeploymentsTotal    int
	DeploymentsHealthy  int
	StatefulSetsTotal   int
	StatefulSetsHealthy int
	DaemonSetsTotal     int
	DaemonSetsHealthy   int
	WorkloadWarnings    []healthFinding
	RecentWarningEvents []healthFinding
}

type healthFinding struct {
	Title string
	Text  string
}

func NewHealthOverviewView(ctx context.Context, state *common.ClusterState) *HealthOverviewView {
	tv, page, refresh, status := toolPage(state, "Cluster Health", "Refresh cluster health")
	view := &HealthOverviewView{
		ToolbarView:  tv,
		ClusterState: state,
		ctx:          ctx,
		page:         page,
		refresh:      refresh,
		status:       status,
	}
	view.status.SetText("Loading…")
	view.refresh.ConnectClicked(view.refreshHealth)
	view.refreshHealth()
	return view
}

func (v *HealthOverviewView) clearGroups() {
	for _, g := range v.groups {
		v.page.Remove(g)
	}
	v.groups = nil
}

func (v *HealthOverviewView) addGroup(g *adw.PreferencesGroup) {
	v.page.Add(g)
	v.groups = append(v.groups, g)
}

func (v *HealthOverviewView) refreshHealth() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing…")
	v.clearGroups()

	go func() {
		overview, err := v.collectHealthOverview()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Failed")
				widget.ShowErrorDialog(v.ctx, "Cluster health failed", err)
				return
			}
			v.status.SetText(fmt.Sprintf("Updated %s", overview.CollectedAt.Format("15:04:05")))
			v.renderHealthOverview(overview)
		})
	}()
}

func (v *HealthOverviewView) collectHealthOverview() (*clusterHealthOverview, error) {
	ctx, cancel := context.WithTimeout(v.ctx, 30*time.Second)
	defer cancel()
	overview := &clusterHealthOverview{CollectedAt: time.Now()}

	var nodes corev1.NodeList
	if err := v.Cluster.List(ctx, &nodes); err != nil {
		return nil, err
	}
	overview.NodesTotal = len(nodes.Items)
	for _, node := range nodes.Items {
		if nodeReady(node) {
			overview.NodesReady++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: node.Name,
			Text:  "Node is not Ready",
		})
	}

	var pods corev1.PodList
	if err := v.Cluster.List(ctx, &pods); err != nil {
		return nil, err
	}
	overview.PodsTotal = len(pods.Items)
	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded:
			overview.PodsRunning++
		case corev1.PodPending:
			overview.PodsPending++
		case corev1.PodFailed:
			overview.PodsFailed++
		}
		restarts := podRestartCount(pod)
		overview.TotalRestarts += restarts
		if finding, ok := podHealthFinding(pod, restarts); ok {
			overview.PodWarnings = append(overview.PodWarnings, finding)
		}
	}

	if err := v.collectWorkloadHealth(ctx, overview); err != nil {
		return nil, err
	}
	if err := v.collectWarningEvents(ctx, overview); err != nil {
		return nil, err
	}

	return overview, nil
}

func (v *HealthOverviewView) collectWorkloadHealth(ctx context.Context, overview *clusterHealthOverview) error {
	var deployments appsv1.DeploymentList
	if err := v.Cluster.List(ctx, &deployments); err != nil {
		return err
	}
	overview.DeploymentsTotal = len(deployments.Items)
	for _, deployment := range deployments.Items {
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		if deployment.Status.AvailableReplicas >= desired {
			overview.DeploymentsHealthy++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: namespacedName(deployment.Namespace, deployment.Name),
			Text:  fmt.Sprintf("Deployment has %d/%d available replicas", deployment.Status.AvailableReplicas, desired),
		})
	}

	var statefulSets appsv1.StatefulSetList
	if err := v.Cluster.List(ctx, &statefulSets); err != nil {
		return err
	}
	overview.StatefulSetsTotal = len(statefulSets.Items)
	for _, statefulSet := range statefulSets.Items {
		desired := int32(1)
		if statefulSet.Spec.Replicas != nil {
			desired = *statefulSet.Spec.Replicas
		}
		if statefulSet.Status.ReadyReplicas >= desired {
			overview.StatefulSetsHealthy++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: namespacedName(statefulSet.Namespace, statefulSet.Name),
			Text:  fmt.Sprintf("StatefulSet has %d/%d ready replicas", statefulSet.Status.ReadyReplicas, desired),
		})
	}

	var daemonSets appsv1.DaemonSetList
	if err := v.Cluster.List(ctx, &daemonSets); err != nil {
		return err
	}
	overview.DaemonSetsTotal = len(daemonSets.Items)
	for _, daemonSet := range daemonSets.Items {
		desired := daemonSet.Status.DesiredNumberScheduled
		if daemonSet.Status.NumberReady >= desired {
			overview.DaemonSetsHealthy++
			continue
		}
		overview.WorkloadWarnings = append(overview.WorkloadWarnings, healthFinding{
			Title: namespacedName(daemonSet.Namespace, daemonSet.Name),
			Text:  fmt.Sprintf("DaemonSet has %d/%d ready pods", daemonSet.Status.NumberReady, desired),
		})
	}

	return nil
}

func (v *HealthOverviewView) collectWarningEvents(ctx context.Context, overview *clusterHealthOverview) error {
	var events corev1.EventList
	if err := v.Cluster.List(ctx, &events); err != nil {
		return err
	}
	sort.Slice(events.Items, func(i, j int) bool {
		return eventTime(events.Items[i]).After(eventTime(events.Items[j]))
	})
	for _, event := range events.Items {
		if event.Type != corev1.EventTypeWarning {
			continue
		}
		overview.RecentWarningEvents = append(overview.RecentWarningEvents, healthFinding{
			Title: fmt.Sprintf("%s %s", event.InvolvedObject.Kind, namespacedName(event.Namespace, event.InvolvedObject.Name)),
			Text:  strings.TrimSpace(fmt.Sprintf("%s: %s", event.Reason, event.Message)),
		})
		if len(overview.RecentWarningEvents) >= 8 {
			break
		}
	}
	return nil
}

func (v *HealthOverviewView) renderHealthOverview(overview *clusterHealthOverview) {
	v.clearGroups()

	// Overview tiles
	overviewGroup := toolGroup("Overview", "Operational state of the cluster at a glance.")
	nodeStyle, _ := healthBadge(overview.NodesReady, overview.NodesTotal)
	podStyle, _ := healthBadge(overview.PodsRunning, overview.PodsTotal)
	pendingStyle := "accent"
	if overview.PodsPending > 0 {
		pendingStyle = "warning"
	}
	failedStyle := "success"
	if overview.PodsFailed > 0 {
		failedStyle = "error"
	}
	restartStyle := "success"
	if overview.TotalRestarts > 0 {
		restartStyle = "warning"
	}
	statTilesGroup(overviewGroup, []statTile{
		{Value: fmt.Sprintf("%d / %d", overview.NodesReady, overview.NodesTotal), Caption: "Nodes ready", Style: nodeStyle},
		{Value: fmt.Sprintf("%d / %d", overview.PodsRunning, overview.PodsTotal), Caption: "Pods running", Style: podStyle},
		{Value: fmt.Sprintf("%d", overview.PodsPending), Caption: "Pending pods", Style: pendingStyle},
		{Value: fmt.Sprintf("%d", overview.PodsFailed), Caption: "Failed pods", Style: failedStyle},
		{Value: fmt.Sprintf("%d", overview.TotalRestarts), Caption: "Container restarts", Style: restartStyle},
	})
	v.addGroup(overviewGroup)

	// Workload health (progress rows)
	workloadGroup := toolGroup("Workloads", "Replica health for Deployments, StatefulSets, and DaemonSets.")
	workloadGroup.Add(progressRow("Deployments",
		fmt.Sprintf("%d / %d healthy", overview.DeploymentsHealthy, overview.DeploymentsTotal),
		float64(overview.DeploymentsHealthy), float64(max(overview.DeploymentsTotal, 1))))
	workloadGroup.Add(progressRow("StatefulSets",
		fmt.Sprintf("%d / %d healthy", overview.StatefulSetsHealthy, overview.StatefulSetsTotal),
		float64(overview.StatefulSetsHealthy), float64(max(overview.StatefulSetsTotal, 1))))
	workloadGroup.Add(progressRow("DaemonSets",
		fmt.Sprintf("%d / %d healthy", overview.DaemonSetsHealthy, overview.DaemonSetsTotal),
		float64(overview.DaemonSetsHealthy), float64(max(overview.DaemonSetsTotal, 1))))
	v.addGroup(workloadGroup)

	// Workload warnings
	warningsGroup := toolGroup("Workload & Node Warnings",
		"Nodes that are not Ready and workloads with missing replicas.")
	if len(overview.WorkloadWarnings) == 0 {
		emptyStatusGroup(warningsGroup, "All workloads healthy",
			"Every node is Ready and all workloads have their desired replicas available.",
			"emblem-default-symbolic")
	} else {
		for _, f := range overview.WorkloadWarnings {
			warningsGroup.Add(findingRow(f.Title, f.Text, "dialog-warning-symbolic", "Warning", "warning"))
		}
	}
	v.addGroup(warningsGroup)

	// Pod issues
	podGroup := toolGroup("Pod Issues",
		"Pods stuck in waiting, failed, or restart-loop states.")
	if len(overview.PodWarnings) == 0 {
		emptyStatusGroup(podGroup, "No pod issues",
			"No pods are crashing, waiting, or restarting.",
			"emblem-default-symbolic")
	} else {
		for _, f := range overview.PodWarnings {
			podGroup.Add(findingRow(f.Title, f.Text, "dialog-warning-symbolic", "Issue", "warning"))
		}
	}
	v.addGroup(podGroup)

	// Recent warning events
	eventsGroup := toolGroup("Recent Warning Events",
		"The most recent Kubernetes Warning events from across the cluster.")
	if len(overview.RecentWarningEvents) == 0 {
		emptyStatusGroup(eventsGroup, "No recent warnings",
			"There are no recent warning events in the cluster.",
			"emblem-default-symbolic")
	} else {
		for _, f := range overview.RecentWarningEvents {
			eventsGroup.Add(findingRow(f.Title, f.Text, "dialog-error-symbolic", "Warning", "error"))
		}
	}
	v.addGroup(eventsGroup)
}

func podRestartCount(pod corev1.Pod) int32 {
	var restarts int32
	for _, status := range pod.Status.ContainerStatuses {
		restarts += status.RestartCount
	}
	return restarts
}

func podHealthFinding(pod corev1.Pod, restarts int32) (healthFinding, bool) {
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodPending {
		return healthFinding{Title: namespacedName(pod.Namespace, pod.Name), Text: fmt.Sprintf("Pod is %s", pod.Status.Phase)}, true
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.State.Waiting != nil {
			return healthFinding{
				Title: namespacedName(pod.Namespace, pod.Name),
				Text:  fmt.Sprintf("%s is waiting: %s", status.Name, status.State.Waiting.Reason),
			}, true
		}
	}
	if restarts > 0 {
		return healthFinding{Title: namespacedName(pod.Namespace, pod.Name), Text: fmt.Sprintf("%d container restarts", restarts)}, true
	}
	return healthFinding{}, false
}

func eventTime(event corev1.Event) time.Time {
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time
	}
	return event.CreationTimestamp.Time
}

func namespacedName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}
