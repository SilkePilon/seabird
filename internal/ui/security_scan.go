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
	corev1 "k8s.io/api/core/v1"
)

type SecurityScanView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	page    *adw.PreferencesPage
	refresh *gtk.Button
	status  *gtk.Label
	groups  []*adw.PreferencesGroup
}

type securityOverview struct {
	Pods             int
	Containers       int
	Services         int
	CriticalFindings int
	HighFindings     int
	MediumFindings   int
	Findings         []securityFinding
}

type securityFinding struct {
	Title    string
	Detail   string
	Severity int
}

func NewSecurityScanView(ctx context.Context, state *common.ClusterState) *SecurityScanView {
	tv, page, refresh, status, _ := toolPage(state, "Security Scan", "Refresh security scan")
	view := &SecurityScanView{
		ToolbarView:  tv,
		ClusterState: state,
		ctx:          ctx,
		page:         page,
		refresh:      refresh,
		status:       status,
	}
	view.status.SetText("Scanning…")
	view.refresh.ConnectClicked(view.refreshSecurity)
	view.refreshSecurity()
	return view
}

func (v *SecurityScanView) clearGroups() {
	for _, g := range v.groups {
		v.page.Remove(g)
	}
	v.groups = nil
}

func (v *SecurityScanView) addGroup(g *adw.PreferencesGroup) {
	v.page.Add(g)
	v.groups = append(v.groups, g)
}

func (v *SecurityScanView) refreshSecurity() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Scanning…")
	v.clearGroups()

	go func() {
		overview, err := v.collectSecurity()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Failed")
				widget.ShowErrorDialog(v.ctx, "Could not run security scan", err)
				return
			}
			v.status.SetText(fmt.Sprintf("Updated %s", time.Now().Format("15:04:05")))
			v.renderSecurity(overview)
		})
	}()
}

func (v *SecurityScanView) collectSecurity() (*securityOverview, error) {
	overview := &securityOverview{}
	var pods corev1.PodList
	if err := v.List(v.ctx, &pods); err != nil {
		return nil, err
	}
	overview.Pods = len(pods.Items)
	for _, pod := range pods.Items {
		scanPodSecurity(&pod, overview)
	}

	var services corev1.ServiceList
	if err := v.List(v.ctx, &services); err != nil {
		return nil, err
	}
	overview.Services = len(services.Items)
	for _, service := range services.Items {
		scanServiceSecurity(&service, overview)
	}

	sort.Slice(overview.Findings, func(i, j int) bool {
		if overview.Findings[i].Severity == overview.Findings[j].Severity {
			return overview.Findings[i].Title < overview.Findings[j].Title
		}
		return overview.Findings[i].Severity > overview.Findings[j].Severity
	})
	for _, finding := range overview.Findings {
		switch {
		case finding.Severity >= 90:
			overview.CriticalFindings++
		case finding.Severity >= 70:
			overview.HighFindings++
		case finding.Severity >= 40:
			overview.MediumFindings++
		}
	}
	return overview, nil
}

func scanPodSecurity(pod *corev1.Pod, overview *securityOverview) {
	podName := pod.Namespace + "/" + pod.Name
	if pod.Spec.HostNetwork {
		addSecurityFinding(overview, podName, "Uses hostNetwork.", 85)
	}
	if pod.Spec.HostPID {
		addSecurityFinding(overview, podName, "Uses hostPID.", 90)
	}
	if pod.Spec.HostIPC {
		addSecurityFinding(overview, podName, "Uses hostIPC.", 80)
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			addSecurityFinding(overview, podName, "Mounts hostPath volume "+volume.Name+".", 80)
		}
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		addSecurityFinding(overview, podName, "Pod does not require runAsNonRoot.", 45)
	}

	for _, container := range pod.Spec.Containers {
		overview.Containers++
		scanContainerSecurity(podName, container, overview)
	}
}

func scanContainerSecurity(podName string, container corev1.Container, overview *securityOverview) {
	name := podName + " · " + container.Name
	if imageUsesLatest(container.Image) {
		addSecurityFinding(overview, name, "Image uses latest or has no explicit tag.", 55)
	}
	if container.SecurityContext == nil {
		addSecurityFinding(overview, name, "Container has no securityContext.", 45)
	} else {
		if container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
			addSecurityFinding(overview, name, "Privileged container.", 95)
		}
		if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
			addSecurityFinding(overview, name, "Privilege escalation is not disabled.", 75)
		}
		if container.SecurityContext.RunAsNonRoot == nil || !*container.SecurityContext.RunAsNonRoot {
			addSecurityFinding(overview, name, "Container does not require runAsNonRoot.", 45)
		}
		if caps := container.SecurityContext.Capabilities; caps != nil && len(caps.Add) > 0 {
			addSecurityFinding(overview, name, "Adds Linux capabilities: "+capabilityList(caps.Add), 70)
		}
	}
	if container.Resources.Requests.Cpu().IsZero() || container.Resources.Requests.Memory().IsZero() {
		addSecurityFinding(overview, name, "Missing CPU or memory request.", 40)
	}
	if container.ReadinessProbe == nil {
		addSecurityFinding(overview, name, "Missing readiness probe.", 35)
	}
	if container.LivenessProbe == nil {
		addSecurityFinding(overview, name, "Missing liveness probe.", 35)
	}
}

func scanServiceSecurity(service *corev1.Service, overview *securityOverview) {
	name := service.Namespace + "/" + service.Name
	switch service.Spec.Type {
	case corev1.ServiceTypeLoadBalancer:
		addSecurityFinding(overview, name, "LoadBalancer service exposes workloads outside the cluster.", 65)
	case corev1.ServiceTypeNodePort:
		addSecurityFinding(overview, name, "NodePort service exposes node-level ports.", 55)
	}
}

func addSecurityFinding(overview *securityOverview, title, detail string, severity int) {
	overview.Findings = append(overview.Findings, securityFinding{Title: title, Detail: detail, Severity: severity})
}

func (v *SecurityScanView) renderSecurity(overview *securityOverview) {
	v.clearGroups()

	scopeGroup := toolGroup("Scope", "Resources scanned in this run.")
	statTilesGroup(scopeGroup, []statTile{
		{Value: fmt.Sprintf("%d", overview.Pods), Caption: "Pods", Style: "accent"},
		{Value: fmt.Sprintf("%d", overview.Containers), Caption: "Containers", Style: "accent"},
		{Value: fmt.Sprintf("%d", overview.Services), Caption: "Services", Style: "accent"},
	})
	v.addGroup(scopeGroup)

	severityGroup := toolGroup("Findings By Severity",
		"How serious the issues are. Critical and High should be addressed soon.")
	statTilesGroup(severityGroup, []statTile{
		{Value: fmt.Sprintf("%d", overview.CriticalFindings), Caption: "Critical", Style: tileStyleForCount(overview.CriticalFindings, true)},
		{Value: fmt.Sprintf("%d", overview.HighFindings), Caption: "High", Style: tileStyleForCount(overview.HighFindings, true)},
		{Value: fmt.Sprintf("%d", overview.MediumFindings), Caption: "Medium", Style: tileStyleForCount(overview.MediumFindings, true)},
	})
	v.addGroup(severityGroup)

	findingsGroup := toolGroup("Findings",
		"Each row is a single best-practice or hardening issue, sorted by severity.")
	if len(overview.Findings) == 0 {
		emptyStatusGroup(findingsGroup, "Nothing to flag",
			"No obvious security or best-practice issues were found.",
			"emblem-default-symbolic")
	} else {
		const limit = 100
		for i := 0; i < min(len(overview.Findings), limit); i++ {
			f := overview.Findings[i]
			style, label, icon := severityClassification(f.Severity)
			finding := f
			findingsGroup.Add(clickableFindingRow(f.Title, f.Detail, icon, label, style, func() {
				v.showFindingDetails(finding)
			}))
		}
		if len(overview.Findings) > limit {
			findingsGroup.Add(metricRow("More",
				fmt.Sprintf("%d additional findings", len(overview.Findings)-limit), ""))
		}
	}
	v.addGroup(findingsGroup)
}

func (v *SecurityScanView) showFindingDetails(f securityFinding) {
	_, label, _ := severityClassification(f.Severity)
	category := "Best practice"
	switch {
	case f.Severity >= 90:
		category = "Critical hardening issue"
	case f.Severity >= 70:
		category = "High-risk configuration"
	case f.Severity >= 40:
		category = "Medium-risk configuration"
	}
	sections := []detailSection{
		{
			Title: "Finding",
			Fields: []detailField{
				{Label: "Resource", Value: f.Title},
				{Label: "Issue", Value: f.Detail},
				{Label: "Category", Value: category},
			},
		},
		{
			Title:       "What this means",
			Description: "This finding is reported when a workload deviates from a Kubernetes hardening best practice. Review the resource's spec and adjust the highlighted setting if appropriate for your environment.",
		},
	}
	showDetailsDialog(v.ctx, fmt.Sprintf("%s — %s", label, f.Title), f.Severity, sections)
}

func imageUsesLatest(image string) bool {
	if strings.HasSuffix(image, ":latest") {
		return true
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	return lastColon < lastSlash
}

func capabilityList(caps []corev1.Capability) string {
	values := make([]string, 0, len(caps))
	for _, cap := range caps {
		values = append(values, string(cap))
	}
	return strings.Join(values, ", ")
}
