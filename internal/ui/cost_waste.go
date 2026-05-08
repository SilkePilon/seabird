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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

type CostWasteView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	page    *adw.PreferencesPage
	refresh *gtk.Button
	status  *gtk.Label
	groups  []*adw.PreferencesGroup
}

type costWasteOverview struct {
	Pods                  int
	Containers            int
	MissingCPURequests    int
	MissingMemoryRequests int
	MissingLimits         int
	IdleCPURequestMilli   int64
	IdleMemoryRequest     int64
	Findings              []costWasteFinding
}

type costWasteFinding struct {
	Title    string
	Detail   string
	Severity int
}

func NewCostWasteView(ctx context.Context, state *common.ClusterState) *CostWasteView {
	tv, page, refresh, status, _ := toolPage(state, "Cost & Waste", "Refresh cost and waste recommendations")
	view := &CostWasteView{
		ToolbarView:  tv,
		ClusterState: state,
		ctx:          ctx,
		page:         page,
		refresh:      refresh,
		status:       status,
	}
	view.status.SetText("Loading…")
	view.refresh.ConnectClicked(view.refreshCostWaste)
	view.refreshCostWaste()
	return view
}

func (v *CostWasteView) clearGroups() {
	for _, g := range v.groups {
		v.page.Remove(g)
	}
	v.groups = nil
}

func (v *CostWasteView) addGroup(g *adw.PreferencesGroup) {
	v.page.Add(g)
	v.groups = append(v.groups, g)
}

func (v *CostWasteView) refreshCostWaste() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing…")
	v.clearGroups()

	go func() {
		overview, err := v.collectCostWaste()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Failed")
				widget.ShowErrorDialog(v.ctx, "Could not load cost recommendations", err)
				return
			}
			v.status.SetText(fmt.Sprintf("Updated %s", time.Now().Format("15:04:05")))
			v.renderCostWaste(overview)
		})
	}()
}

func (v *CostWasteView) collectCostWaste() (*costWasteOverview, error) {
	var pods corev1.PodList
	if err := v.List(v.ctx, &pods); err != nil {
		return nil, err
	}

	overview := &costWasteOverview{Pods: len(pods.Items)}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		metrics := v.Metrics.Pod(types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace})
		if pod.Status.QOSClass == corev1.PodQOSBestEffort {
			overview.Findings = append(overview.Findings, costWasteFinding{
				Title:    pod.Namespace + "/" + pod.Name,
				Detail:   "BestEffort pod has no requests, so scheduling and cost attribution are unreliable.",
				Severity: 70,
			})
		}

		for _, container := range pod.Spec.Containers {
			overview.Containers++
			name := pod.Namespace + "/" + pod.Name + " · " + container.Name
			cpuRequest := container.Resources.Requests.Cpu()
			memoryRequest := container.Resources.Requests.Memory()
			if cpuRequest == nil || cpuRequest.IsZero() {
				overview.MissingCPURequests++
				overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: "Missing CPU request.", Severity: 60})
			}
			if memoryRequest == nil || memoryRequest.IsZero() {
				overview.MissingMemoryRequests++
				overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: "Missing memory request.", Severity: 60})
			}
			if container.Resources.Limits.Cpu().IsZero() && container.Resources.Limits.Memory().IsZero() {
				overview.MissingLimits++
			}

			if metrics == nil {
				continue
			}
			for _, containerMetrics := range metrics.Containers {
				if containerMetrics.Name != container.Name {
					continue
				}
				if cpuRequest != nil && !cpuRequest.IsZero() {
					requestMilli := cpuRequest.MilliValue()
					usageMilli := containerMetrics.Usage.Cpu().MilliValue()
					if requestMilli >= 100 && usageMilli*5 < requestMilli {
						overview.IdleCPURequestMilli += requestMilli - usageMilli
						overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: fmt.Sprintf("CPU request %dm, current usage %dm.", requestMilli, usageMilli), Severity: 45})
					}
				}
				if memoryRequest != nil && !memoryRequest.IsZero() {
					requestBytes := memoryRequest.Value()
					usageBytes := containerMetrics.Usage.Memory().Value()
					if requestBytes >= 128*1024*1024 && usageBytes*3 < requestBytes {
						overview.IdleMemoryRequest += requestBytes - usageBytes
						overview.Findings = append(overview.Findings, costWasteFinding{Title: name, Detail: fmt.Sprintf("Memory request %s, current usage %s.", bytesQuantity(requestBytes), bytesQuantity(usageBytes)), Severity: 45})
					}
				}
			}
		}
	}
	sort.Slice(overview.Findings, func(i, j int) bool {
		if overview.Findings[i].Severity == overview.Findings[j].Severity {
			return overview.Findings[i].Title < overview.Findings[j].Title
		}
		return overview.Findings[i].Severity > overview.Findings[j].Severity
	})
	return overview, nil
}

func (v *CostWasteView) renderCostWaste(overview *costWasteOverview) {
	v.clearGroups()

	overviewGroup := toolGroup("Overview", "Workload coverage and headline waste indicators.")
	statTilesGroup(overviewGroup, []statTile{
		{Value: fmt.Sprintf("%d", overview.Pods), Caption: "Pods", Style: "accent"},
		{Value: fmt.Sprintf("%d", overview.Containers), Caption: "Containers", Style: "accent"},
		{Value: fmt.Sprintf("%d", len(overview.Findings)), Caption: "Recommendations", Style: tileStyleForCount(len(overview.Findings), true)},
	})
	v.addGroup(overviewGroup)

	requestsGroup := toolGroup("Requests & Limits", "Coverage of CPU and memory requests and limits.")
	requestsGroup.Add(metricRow("Missing CPU requests", "Containers without spec.resources.requests.cpu",
		fmt.Sprintf("%d", overview.MissingCPURequests)))
	requestsGroup.Add(metricRow("Missing memory requests", "Containers without spec.resources.requests.memory",
		fmt.Sprintf("%d", overview.MissingMemoryRequests)))
	requestsGroup.Add(metricRow("Missing CPU and memory limits", "Containers without any limits set",
		fmt.Sprintf("%d", overview.MissingLimits)))
	v.addGroup(requestsGroup)

	wasteGroup := toolGroup("Potential Waste", "Reserved capacity that current usage does not justify.")
	wasteGroup.Add(metricRow("Idle CPU request", "Sum across containers using less than 1/5 of their request",
		fmt.Sprintf("%dm", overview.IdleCPURequestMilli)))
	wasteGroup.Add(metricRow("Idle memory request", "Sum across containers using less than 1/3 of their request",
		bytesQuantity(overview.IdleMemoryRequest)))
	v.addGroup(wasteGroup)

	findingsGroup := toolGroup("Recommendations",
		"Actionable hints sorted by severity. Highest impact first.")
	if len(overview.Findings) == 0 {
		emptyStatusGroup(findingsGroup, "Nothing to recommend",
			"Requests and current metrics do not show obvious waste.",
			"emblem-default-symbolic")
	} else {
		const limit = 80
		for i := 0; i < min(len(overview.Findings), limit); i++ {
			f := overview.Findings[i]
			style, label, icon := severityClassification(f.Severity)
			finding := f
			findingsGroup.Add(clickableFindingRow(f.Title, f.Detail, icon, label, style, func() {
				v.showRecommendationDetails(finding)
			}))
		}
		if len(overview.Findings) > limit {
			findingsGroup.Add(metricRow("More",
				fmt.Sprintf("%d additional recommendations", len(overview.Findings)-limit), ""))
		}
	}
	v.addGroup(findingsGroup)
}

func (v *CostWasteView) showRecommendationDetails(f costWasteFinding) {
	_, label, _ := severityClassification(f.Severity)
	sections := []detailSection{
		{
			Title: "Recommendation",
			Fields: []detailField{
				{Label: "Workload", Value: f.Title},
				{Label: "Detail", Value: f.Detail},
			},
		},
		{
			Title:       "How to act",
			Description: "Adjust spec.resources.requests/limits on the affected container, or right-size the workload based on the observed usage. Idle requests reserve cluster capacity that other pods cannot use.",
		},
	}
	showDetailsDialog(v.ctx, fmt.Sprintf("%s — %s", label, f.Title), f.Severity, sections)
}

func bytesQuantity(value int64) string {
	quantity := resource.NewQuantity(value, resource.BinarySI)
	return quantity.String()
}
