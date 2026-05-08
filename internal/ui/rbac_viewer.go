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
	rbacv1 "k8s.io/api/rbac/v1"
)

type RBACViewer struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	page    *adw.PreferencesPage
	refresh *gtk.Button
	status  *gtk.Label
	groups  []*adw.PreferencesGroup
}

type rbacOverview struct {
	Roles               []rbacv1.Role
	ClusterRoles        []rbacv1.ClusterRole
	RoleBindings        []rbacv1.RoleBinding
	ClusterRoleBindings []rbacv1.ClusterRoleBinding
}

func NewRBACViewer(ctx context.Context, state *common.ClusterState) *RBACViewer {
	tv, page, refresh, status := toolPage(state, "RBAC", "Refresh RBAC overview")
	view := &RBACViewer{
		ToolbarView:  tv,
		ClusterState: state,
		ctx:          ctx,
		page:         page,
		refresh:      refresh,
		status:       status,
	}
	view.status.SetText("Loading…")
	view.refresh.ConnectClicked(view.refreshRBAC)
	view.refreshRBAC()
	return view
}

func (v *RBACViewer) clearGroups() {
	for _, g := range v.groups {
		v.page.Remove(g)
	}
	v.groups = nil
}

func (v *RBACViewer) addGroup(g *adw.PreferencesGroup) {
	v.page.Add(g)
	v.groups = append(v.groups, g)
}

func (v *RBACViewer) refreshRBAC() {
	v.refresh.SetSensitive(false)
	v.status.SetText("Refreshing…")
	v.clearGroups()

	go func() {
		overview, err := v.collectRBAC()
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Failed")
				widget.ShowErrorDialog(v.ctx, "Could not load RBAC", err)
				return
			}
			v.status.SetText(fmt.Sprintf("Updated %s", time.Now().Format("15:04:05")))
			v.renderRBAC(overview)
		})
	}()
}

func (v *RBACViewer) collectRBAC() (*rbacOverview, error) {
	overview := &rbacOverview{}
	var roles rbacv1.RoleList
	if err := v.List(v.ctx, &roles); err != nil {
		return nil, err
	}
	overview.Roles = roles.Items

	var clusterRoles rbacv1.ClusterRoleList
	if err := v.List(v.ctx, &clusterRoles); err != nil {
		return nil, err
	}
	overview.ClusterRoles = clusterRoles.Items

	var bindings rbacv1.RoleBindingList
	if err := v.List(v.ctx, &bindings); err != nil {
		return nil, err
	}
	overview.RoleBindings = bindings.Items

	var clusterBindings rbacv1.ClusterRoleBindingList
	if err := v.List(v.ctx, &clusterBindings); err != nil {
		return nil, err
	}
	overview.ClusterRoleBindings = clusterBindings.Items
	return overview, nil
}

type roleFinding struct {
	Name        string
	Text        string
	Kind        string
	Namespace   string
	Rules       []rbacv1.PolicyRule
	Annotations map[string]string
	Labels      map[string]string
}

type bindingRowData struct {
	Name      string
	Namespace string
	Kind      string
	Subjects  []rbacv1.Subject
	RoleRef   rbacv1.RoleRef
}

func (v *RBACViewer) renderRBAC(overview *rbacOverview) {
	v.clearGroups()

	// Overview tiles
	overviewGroup := toolGroup("Overview", "Roles, ClusterRoles, and bindings discovered in the cluster.")
	statTilesGroup(overviewGroup, []statTile{
		{Value: fmt.Sprintf("%d", len(overview.Roles)), Caption: "Roles", Style: "accent"},
		{Value: fmt.Sprintf("%d", len(overview.ClusterRoles)), Caption: "ClusterRoles", Style: "accent"},
		{Value: fmt.Sprintf("%d", len(overview.RoleBindings)), Caption: "RoleBindings", Style: "accent"},
		{Value: fmt.Sprintf("%d", len(overview.ClusterRoleBindings)), Caption: "ClusterRoleBindings", Style: "accent"},
	})
	v.addGroup(overviewGroup)

	// Broad-access roles
	var broad []roleFinding
	for _, role := range overview.Roles {
		if text := broadRuleSummary(role.Rules); text != "" {
			broad = append(broad, roleFinding{
				Name: role.Namespace + "/" + role.Name, Text: text,
				Kind: "Role", Namespace: role.Namespace, Rules: role.Rules,
				Annotations: role.Annotations, Labels: role.Labels,
			})
		}
	}
	for _, role := range overview.ClusterRoles {
		if text := broadRuleSummary(role.Rules); text != "" {
			broad = append(broad, roleFinding{
				Name: role.Name, Text: text,
				Kind: "ClusterRole", Rules: role.Rules,
				Annotations: role.Annotations, Labels: role.Labels,
			})
		}
	}
	sort.Slice(broad, func(i, j int) bool { return broad[i].Name < broad[j].Name })

	broadGroup := toolGroup("Roles With Broad Access",
		"Roles and ClusterRoles that allow wildcard verbs or resources.")
	if len(broad) == 0 {
		emptyStatusGroup(broadGroup, "No broad roles",
			"No wildcard admin-style rules were found.",
			"emblem-default-symbolic")
	} else {
		const limit = 40
		for i, f := range broad {
			if i >= limit {
				broadGroup.Add(metricRow("More",
					fmt.Sprintf("%d additional broad roles", len(broad)-i), ""))
				break
			}
			finding := f
			broadGroup.Add(clickableFindingRow(f.Name, f.Text, "dialog-warning-symbolic", "Broad", "warning", func() {
				v.showRoleDetails(finding)
			}))
		}
	}
	v.addGroup(broadGroup)

	// Bindings
	var bindings []bindingRowData
	for _, b := range overview.RoleBindings {
		bindings = append(bindings, bindingRowData{
			Name:      b.Namespace + "/" + b.Name,
			Namespace: b.Namespace,
			Kind:      "RoleBinding",
			Subjects:  b.Subjects,
			RoleRef:   b.RoleRef,
		})
	}
	for _, b := range overview.ClusterRoleBindings {
		bindings = append(bindings, bindingRowData{
			Name:     b.Name,
			Kind:     "ClusterRoleBinding",
			Subjects: b.Subjects,
			RoleRef:  b.RoleRef,
		})
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Name < bindings[j].Name })

	bindingGroup := toolGroup("Bindings",
		"Each binding shows its subjects and the role it grants. Click for full details.")
	if len(bindings) == 0 {
		emptyStatusGroup(bindingGroup, "No bindings",
			"No RBAC bindings were found in the cluster.",
			"emblem-default-symbolic")
	} else {
		const limit = 80
		for i, b := range bindings {
			if i >= limit {
				bindingGroup.Add(metricRow("More",
					fmt.Sprintf("%d additional bindings", len(bindings)-i), ""))
				break
			}
			binding := b
			subtitle := fmt.Sprintf("%s → %s · %s", b.Kind, b.RoleRef.Kind, b.RoleRef.Name)
			detail := subjectSummary(b.Subjects)
			bindingGroup.Add(clickableFindingRow(b.Name, subtitle+"\n"+detail, "dialog-password-symbolic", b.Kind, "accent", func() {
				v.showBindingDetails(binding)
			}))
		}
	}
	v.addGroup(bindingGroup)
}

func (v *RBACViewer) showRoleDetails(f roleFinding) {
	rulesText := strings.Join(formatRules(f.Rules), "\n\n")
	if rulesText == "" {
		rulesText = "No rules."
	}
	fields := []detailField{
		{Label: "Kind", Value: f.Kind},
		{Label: "Name", Value: f.Name},
	}
	if f.Namespace != "" {
		fields = append(fields, detailField{Label: "Namespace", Value: f.Namespace})
	}
	fields = append(fields, detailField{Label: "Why broad", Value: f.Text})

	sections := []detailSection{
		{Title: "Role", Fields: fields},
		{Title: "Rules", Fields: []detailField{{Label: "Policy rules", Value: rulesText}}},
	}
	if len(f.Labels) > 0 {
		sections = append(sections, detailSection{Title: "Labels", Fields: kvFields(f.Labels)})
	}
	if len(f.Annotations) > 0 {
		sections = append(sections, detailSection{Title: "Annotations", Fields: kvFields(f.Annotations)})
	}
	showDetailsDialog(v.ctx, f.Name, 70, sections)
}

func (v *RBACViewer) showBindingDetails(b bindingRowData) {
	fields := []detailField{
		{Label: "Kind", Value: b.Kind},
		{Label: "Name", Value: b.Name},
	}
	if b.Namespace != "" {
		fields = append(fields, detailField{Label: "Namespace", Value: b.Namespace})
	}
	fields = append(fields,
		detailField{Label: "Role kind", Value: b.RoleRef.Kind},
		detailField{Label: "Role name", Value: b.RoleRef.Name},
		detailField{Label: "Role apiGroup", Value: b.RoleRef.APIGroup},
	)

	subjectFields := make([]detailField, 0, len(b.Subjects))
	for _, s := range b.Subjects {
		name := s.Name
		if s.Namespace != "" {
			name = s.Namespace + "/" + name
		}
		subjectFields = append(subjectFields, detailField{Label: s.Kind, Value: name})
	}
	if len(subjectFields) == 0 {
		subjectFields = append(subjectFields, detailField{Label: "Subjects", Value: "None"})
	}

	sections := []detailSection{
		{Title: "Binding", Fields: fields},
		{Title: fmt.Sprintf("Subjects (%d)", len(b.Subjects)), Fields: subjectFields},
	}
	showDetailsDialog(v.ctx, b.Name, 0, sections)
}

func formatRules(rules []rbacv1.PolicyRule) []string {
	out := make([]string, 0, len(rules))
	for i, rule := range rules {
		var parts []string
		if len(rule.Verbs) > 0 {
			parts = append(parts, "verbs: "+strings.Join(rule.Verbs, ", "))
		}
		if len(rule.APIGroups) > 0 {
			parts = append(parts, "apiGroups: "+strings.Join(rule.APIGroups, ", "))
		}
		if len(rule.Resources) > 0 {
			parts = append(parts, "resources: "+strings.Join(rule.Resources, ", "))
		}
		if len(rule.ResourceNames) > 0 {
			parts = append(parts, "resourceNames: "+strings.Join(rule.ResourceNames, ", "))
		}
		if len(rule.NonResourceURLs) > 0 {
			parts = append(parts, "nonResourceURLs: "+strings.Join(rule.NonResourceURLs, ", "))
		}
		out = append(out, fmt.Sprintf("[%d] %s", i+1, strings.Join(parts, "\n     ")))
	}
	return out
}

func kvFields(m map[string]string) []detailField {
	out := make([]detailField, 0, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, detailField{Label: k, Value: m[k]})
	}
	return out
}

func broadRuleSummary(rules []rbacv1.PolicyRule) string {
	for _, rule := range rules {
		if containsString(rule.Verbs, "*") && (containsString(rule.Resources, "*") || containsString(rule.NonResourceURLs, "*")) {
			return "Wildcard verbs and resources"
		}
		if containsString(rule.Verbs, "*") {
			return "Wildcard verbs on " + strings.Join(rule.Resources, ", ")
		}
	}
	return ""
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func subjectSummary(subjects []rbacv1.Subject) string {
	if len(subjects) == 0 {
		return "No subjects"
	}
	parts := make([]string, 0, min(len(subjects), 3))
	for i, subject := range subjects {
		if i >= 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(subjects)-i))
			break
		}
		name := subject.Name
		if subject.Namespace != "" {
			name = subject.Namespace + "/" + name
		}
		parts = append(parts, subject.Kind+":"+name)
	}
	return strings.Join(parts, ", ")
}
