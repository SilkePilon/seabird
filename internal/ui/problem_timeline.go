package ui

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

type ProblemTimelineView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	page    *adw.PreferencesPage
	refresh *gtk.Button
	status  *gtk.Label
	groups  []*adw.PreferencesGroup
}

type timelineEntry struct {
	When     time.Time
	Severity string
	Title    string
	Text     string
}

func NewProblemTimelineView(ctx context.Context, state *common.ClusterState) *ProblemTimelineView {
	tv, page, refresh, status := toolPage(state, "Problem Timeline", "Refresh problem timeline")
	view := &ProblemTimelineView{
		ToolbarView:  tv,
		ClusterState: state,
		ctx:          ctx,
		page:         page,
		refresh:      refresh,
		status:       status,
	}
	view.status.SetText("Loading…")
	view.refresh.ConnectClicked(view.refreshTimeline)
	view.refreshTimeline()
	return view
}

func (v *ProblemTimelineView) clearGroups() {
	for _, g := range v.groups {
		v.page.Remove(g)
	}
	v.groups = nil
}

func (v *ProblemTimelineView) addGroup(g *adw.PreferencesGroup) {
	v.page.Add(g)
	v.groups = append(v.groups, g)
}

func (v *ProblemTimelineView) refreshTimeline() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing…")
	v.clearGroups()

	go func() {
		entries, err := v.collectProblemTimeline()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Failed")
				widget.ShowErrorDialog(v.ctx, "Problem timeline failed", err)
				return
			}
			v.status.SetText(fmt.Sprintf("Updated %s", time.Now().Format("15:04:05")))
			v.renderTimeline(entries)
		})
	}()
}

func (v *ProblemTimelineView) collectProblemTimeline() ([]timelineEntry, error) {
	ctx, cancel := context.WithTimeout(v.ctx, 30*time.Second)
	defer cancel()

	var entries []timelineEntry
	var events corev1.EventList
	if err := v.Cluster.List(ctx, &events); err != nil {
		return nil, err
	}
	for _, event := range events.Items {
		severity := "Info"
		if event.Type == corev1.EventTypeWarning {
			severity = "Warning"
		}
		entries = append(entries, timelineEntry{
			When:     eventTime(event),
			Severity: severity,
			Title:    fmt.Sprintf("%s %s", event.InvolvedObject.Kind, namespacedName(event.Namespace, event.InvolvedObject.Name)),
			Text:     fmt.Sprintf("%s: %s", event.Reason, event.Message),
		})
	}

	var pods corev1.PodList
	if err := v.Cluster.List(ctx, &pods); err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		restarts := podRestartCount(pod)
		if restarts > 0 {
			entries = append(entries, timelineEntry{
				When:     pod.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(pod.Namespace, pod.Name),
				Text:     fmt.Sprintf("%d container restarts observed", restarts),
			})
		}
		if finding, ok := podHealthFinding(pod, restarts); ok {
			entries = append(entries, timelineEntry{
				When:     pod.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    finding.Title,
				Text:     finding.Text,
			})
		}
	}

	if err := v.appendRolloutTimeline(ctx, &entries); err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].When.After(entries[j].When) })
	if len(entries) > 80 {
		entries = entries[:80]
	}
	return entries, nil
}

func (v *ProblemTimelineView) appendRolloutTimeline(ctx context.Context, entries *[]timelineEntry) error {
	var deployments appsv1.DeploymentList
	if err := v.Cluster.List(ctx, &deployments); err != nil {
		return err
	}
	for _, deployment := range deployments.Items {
		desired := int32(1)
		if deployment.Spec.Replicas != nil {
			desired = *deployment.Spec.Replicas
		}
		if deployment.Status.AvailableReplicas < desired {
			*entries = append(*entries, timelineEntry{
				When:     deployment.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(deployment.Namespace, deployment.Name),
				Text:     fmt.Sprintf("Deployment availability is %d/%d", deployment.Status.AvailableReplicas, desired),
			})
		}
	}

	var statefulSets appsv1.StatefulSetList
	if err := v.Cluster.List(ctx, &statefulSets); err != nil {
		return err
	}
	for _, statefulSet := range statefulSets.Items {
		desired := int32(1)
		if statefulSet.Spec.Replicas != nil {
			desired = *statefulSet.Spec.Replicas
		}
		if statefulSet.Status.ReadyReplicas < desired {
			*entries = append(*entries, timelineEntry{
				When:     statefulSet.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(statefulSet.Namespace, statefulSet.Name),
				Text:     fmt.Sprintf("StatefulSet readiness is %d/%d", statefulSet.Status.ReadyReplicas, desired),
			})
		}
	}

	var daemonSets appsv1.DaemonSetList
	if err := v.Cluster.List(ctx, &daemonSets); err != nil {
		return err
	}
	for _, daemonSet := range daemonSets.Items {
		desired := daemonSet.Status.DesiredNumberScheduled
		if daemonSet.Status.NumberReady < desired {
			*entries = append(*entries, timelineEntry{
				When:     daemonSet.CreationTimestamp.Time,
				Severity: "Warning",
				Title:    namespacedName(daemonSet.Namespace, daemonSet.Name),
				Text:     fmt.Sprintf("DaemonSet readiness is %d/%d", daemonSet.Status.NumberReady, desired),
			})
		}
	}
	return nil
}

func (v *ProblemTimelineView) renderTimeline(entries []timelineEntry) {
	v.clearGroups()

	warnings := 0
	for _, e := range entries {
		if e.Severity == "Warning" {
			warnings++
		}
	}
	infos := len(entries) - warnings

	overview := toolGroup("Overview", "Recent Kubernetes events, pod restarts, and rollout health changes.")
	statTilesGroup(overview, []statTile{
		{Value: fmt.Sprintf("%d", len(entries)), Caption: "Signals", Style: "accent"},
		{Value: fmt.Sprintf("%d", warnings), Caption: "Warnings", Style: tileStyleForCount(warnings, true)},
		{Value: fmt.Sprintf("%d", infos), Caption: "Informational", Style: "accent"},
	})
	v.addGroup(overview)

	if len(entries) == 0 {
		empty := toolGroup("Latest Signals", "")
		emptyStatusGroup(empty, "All clear",
			"There are no recent problems, restarts, or warning events.",
			"emblem-default-symbolic")
		v.addGroup(empty)
		return
	}

	signals := toolGroup("Latest Signals",
		fmt.Sprintf("Showing %d most recent items, newest first.", len(entries)))
	for _, entry := range entries {
		style := "warning"
		icon := "dialog-warning-symbolic"
		if entry.Severity != "Warning" {
			style = "accent"
			icon = "dialog-information-symbolic"
		}
		title := fmt.Sprintf("%s — %s", entry.When.Format("Jan 2 15:04"), entry.Title)
		e := entry
		signals.Add(clickableFindingRow(title, entry.Text, icon, entry.Severity, style, func() {
			v.showSignalDetails(e)
		}))
	}
	v.addGroup(signals)
}

func (v *ProblemTimelineView) showSignalDetails(e timelineEntry) {
	age := "unknown"
	if !e.When.IsZero() {
		age = humanDuration(time.Since(e.When)) + " ago"
	}
	sections := []detailSection{
		{
			Title: "Signal",
			Fields: []detailField{
				{Label: "Source", Value: e.Title},
				{Label: "Severity", Value: e.Severity},
				{Label: "When", Value: e.When.Format("2006-01-02 15:04:05")},
				{Label: "Age", Value: age},
			},
		},
		{
			Title: "Details",
			Fields: []detailField{
				{Label: "Message", Value: e.Text},
			},
		},
	}
	severity := 30
	if e.Severity == "Warning" {
		severity = 70
	}
	showDetailsDialog(v.ctx, e.Title, severity, sections)
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func tileStyleForCount(n int, badIfPositive bool) string {
	if n == 0 {
		return "success"
	}
	if badIfPositive {
		return "warning"
	}
	return "accent"
}
