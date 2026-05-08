package ui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SilkePilon/Orchestrator/internal/apps"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/ui/common"
	"github.com/SilkePilon/Orchestrator/widget"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotk4/pkg/pango"
)

// AppsView shows the Portainer-template catalog and lets the user install
// templates as Kubernetes workloads.
type AppsView struct {
	*adw.ToolbarView
	*common.ClusterState
	ctx     context.Context
	page    *adw.PreferencesPage
	refresh *gtk.Button
	status  *gtk.Label

	catalog        *apps.Catalog
	installedSlugs map[string]struct{}

	searchEntry   *gtk.SearchEntry
	categoryDrop  *gtk.DropDown
	categoryModel *gtk.StringList
	allCategory   string

	filtersGroup *adw.PreferencesGroup
	templatesGrp *adw.PreferencesGroup
	infoGrp      *adw.PreferencesGroup
}

func NewAppsView(ctx context.Context, state *common.ClusterState) *AppsView {
	tv, page, refresh, status, header := toolPage(state, "App Catalog", "Reload templates")
	v := &AppsView{
		ToolbarView:    tv,
		ClusterState:   state,
		ctx:            ctx,
		page:           page,
		refresh:        refresh,
		status:         status,
		installedSlugs: map[string]struct{}{},
		allCategory:    "All categories",
	}
	v.status.SetText("Loading templates…")
	v.refresh.ConnectClicked(func() { v.loadCatalog(true) })

	importBtn := gtk.NewButtonFromIconName("document-new-symbolic")
	importBtn.AddCSSClass("flat")
	importBtn.SetTooltipText("Import a custom app from YAML / JSON")
	importBtn.ConnectClicked(func() { v.openImportDialog() })
	header.PackStart(importBtn)

	v.loadCatalog(false)
	return v
}

func (v *AppsView) clearGroups() {
	for _, g := range []*adw.PreferencesGroup{v.infoGrp, v.filtersGroup, v.templatesGrp} {
		if g != nil {
			v.page.Remove(g)
		}
	}
	v.infoGrp = nil
	v.filtersGroup = nil
	v.templatesGrp = nil
}

func (v *AppsView) loadCatalog(force bool) {
	v.refresh.SetSensitive(false)
	v.status.SetText("Loading…")
	v.clearGroups()

	go func() {
		cat, err := apps.LoadCatalog(v.ctx, force)
		var installed []string
		if err == nil {
			installed, _ = apps.ListInstalledSlugs(v.ctx, v.ClusterState.Cluster.Client)
		}
		glib.IdleAdd(func() {
			v.refresh.SetSensitive(true)
			if err != nil {
				v.status.SetText("Failed")
				widget.ShowErrorDialog(v.ctx, "Could not load app catalog", err)
				return
			}
			v.catalog = cat
			v.installedSlugs = map[string]struct{}{}
			for _, s := range installed {
				v.installedSlugs[s] = struct{}{}
			}
			v.status.SetText(fmt.Sprintf("%d templates · %s", len(cat.Templates), cat.FetchedAt.Format("15:04:05")))
			v.renderAll()
		})
	}()
}

func (v *AppsView) renderAll() {
	v.clearGroups()

	v.infoGrp = toolGroup("Catalog", "Install ready-made services from the Portainer community templates.")
	statTilesGroup(v.infoGrp, []statTile{
		{Value: fmt.Sprintf("%d", len(v.catalog.Templates)), Caption: "Templates", Style: "accent"},
		{Value: fmt.Sprintf("%d", v.countByType(1)+v.countByType(4)), Caption: "Single-container", Style: "accent"},
		{Value: fmt.Sprintf("%d", v.countByType(2)+v.countByType(3)), Caption: "Compose stacks", Style: "accent"},
		{Value: fmt.Sprintf("%d", len(v.installedSlugs)), Caption: "Installed", Style: "success"},
	})
	v.page.Add(v.infoGrp)

	v.filtersGroup = toolGroup("Filters", "Search and filter templates by name or category.")

	searchRow := adw.NewActionRow()
	searchRow.SetTitle("Search")
	v.searchEntry = gtk.NewSearchEntry()
	v.searchEntry.SetHExpand(true)
	v.searchEntry.SetVAlign(gtk.AlignCenter)
	v.searchEntry.SetPlaceholderText("Filter by name or description…")
	v.searchEntry.ConnectSearchChanged(func() { v.renderTemplates() })
	searchRow.AddSuffix(v.searchEntry)
	v.filtersGroup.Add(searchRow)

	categories := append([]string{v.allCategory}, v.catalog.Categories()...)
	v.categoryModel = gtk.NewStringList(categories)
	v.categoryDrop = gtk.NewDropDown(v.categoryModel, nil)
	v.categoryDrop.SetVAlign(gtk.AlignCenter)
	v.categoryDrop.Connect("notify::selected", func() { v.renderTemplates() })

	catRow := adw.NewActionRow()
	catRow.SetTitle("Category")
	catRow.AddSuffix(v.categoryDrop)
	v.filtersGroup.Add(catRow)

	v.page.Add(v.filtersGroup)

	v.renderTemplates()
}

func (v *AppsView) countByType(t int) int {
	n := 0
	for _, x := range v.catalog.Templates {
		if x.Type == t {
			n++
		}
	}
	return n
}

func (v *AppsView) renderTemplates() {
	if v.templatesGrp != nil {
		v.page.Remove(v.templatesGrp)
		v.templatesGrp = nil
	}
	v.templatesGrp = toolGroup("Templates", "Click an entry to view details and install.")

	query := strings.ToLower(strings.TrimSpace(v.searchEntry.Text()))
	selectedCat := v.allCategory
	if v.categoryDrop != nil && v.categoryModel != nil {
		idx := v.categoryDrop.Selected()
		if int(idx) < int(v.categoryModel.NItems()) {
			selectedCat = v.categoryModel.String(idx)
		}
	}

	const limit = 200
	matches := 0
	shown := 0
	for i := range v.catalog.Templates {
		t := v.catalog.Templates[i]
		if !v.templateMatches(t, query, selectedCat) {
			continue
		}
		matches++
		if shown >= limit {
			continue
		}
		shown++
		row := v.buildTemplateRow(t)
		v.templatesGrp.Add(row)
	}

	if matches == 0 {
		emptyStatusGroup(v.templatesGrp, "No matches",
			"Try a different search term or clear the category filter.",
			"system-search-symbolic")
	} else if matches > shown {
		more := metricRow("More", fmt.Sprintf("%d additional templates not shown", matches-shown), "")
		v.templatesGrp.Add(more)
	}

	v.page.Add(v.templatesGrp)
}

func (v *AppsView) templateMatches(t apps.Template, query, selectedCat string) bool {
	if selectedCat != "" && selectedCat != v.allCategory {
		found := false
		for _, c := range t.Categories {
			if strings.EqualFold(c, selectedCat) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if query == "" {
		return true
	}
	hay := strings.ToLower(t.DisplayName() + " " + t.Description + " " + t.Image + " " + strings.Join(t.Categories, " "))
	for _, term := range strings.Fields(query) {
		if !strings.Contains(hay, term) {
			return false
		}
	}
	return true
}

func (v *AppsView) buildTemplateRow(t apps.Template) *adw.ActionRow {
	row := adw.NewActionRow()
	row.SetTitle(escapeMarkup(t.DisplayName()))
	subtitle := t.Description
	if t.CategoriesText() != "" {
		if subtitle != "" {
			subtitle = t.CategoriesText() + " · " + subtitle
		} else {
			subtitle = t.CategoriesText()
		}
	}
	row.SetSubtitle(escapeMarkup(subtitle))
	row.SetSubtitleLines(2)

	icon := gtk.NewImageFromIconName(iconForType(t.Type))
	icon.AddCSSClass("dim-label")
	row.AddPrefix(icon)

	if _, ok := v.installedSlugs[t.Slug()]; ok {
		pill := gtk.NewLabel("Installed")
		pill.AddCSSClass("success")
		pill.AddCSSClass("pill")
		pill.SetVAlign(gtk.AlignCenter)
		row.AddSuffix(pill)
	}
	pill := gtk.NewLabel(t.TypeLabel())
	pill.AddCSSClass("accent")
	pill.AddCSSClass("pill")
	pill.SetVAlign(gtk.AlignCenter)
	row.AddSuffix(pill)

	chevron := gtk.NewImageFromIconName("go-next-symbolic")
	chevron.AddCSSClass("dim-label")
	row.AddSuffix(chevron)
	row.SetActivatable(true)
	row.ConnectActivated(func() { v.openDrawer(t) })
	return row
}

func iconForType(t int) string {
	switch t {
	case 1:
		return "package-x-generic-symbolic"
	case 2, 3:
		return "view-grid-symbolic"
	case 4:
		return "system-software-install-symbolic"
	}
	return "application-x-executable-symbolic"
}

func escapeMarkup(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// ---------------- detail drawer ----------------

func (v *AppsView) openDrawer(t apps.Template) {
	parent := ctxt.MustFrom[*gtk.Window](v.ctx)

	dialog := adw.NewDialog()
	dialog.SetTitle(t.DisplayName())
	dialog.SetContentWidth(720)
	dialog.SetContentHeight(640)

	tv := adw.NewToolbarView()
	tv.AddCSSClass("view")
	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle(t.DisplayName(), t.TypeLabel()))
	tv.AddTopBar(header)

	installBtn := gtk.NewButtonWithLabel("Install")
	installBtn.AddCSSClass("suggested-action")
	uninstallBtn := gtk.NewButtonWithLabel("Uninstall")
	uninstallBtn.AddCSSClass("destructive-action")
	header.PackStart(uninstallBtn)
	header.PackEnd(installBtn)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	page := adw.NewPreferencesPage()

	overview := adw.NewPreferencesGroup()
	overview.SetTitle("Overview")
	descRow := adw.NewActionRow()
	descRow.SetTitle("Description")
	descRow.SetSubtitle(escapeMarkup(coalesce(t.Description, "—")))
	descRow.SetSubtitleLines(0)
	descRow.SetSubtitleSelectable(true)
	overview.Add(descRow)

	typeRow := adw.NewActionRow()
	typeRow.SetTitle("Type")
	typeRow.SetSubtitle(t.TypeLabel())
	overview.Add(typeRow)

	if t.Image != "" {
		r := adw.NewActionRow()
		r.SetTitle("Image")
		r.SetSubtitle(t.Image)
		r.SetSubtitleSelectable(true)
		overview.Add(r)
	}
	if t.Maintainer != "" {
		r := adw.NewActionRow()
		r.SetTitle("Maintainer")
		r.SetSubtitle(strings.TrimSpace(t.Maintainer))
		r.SetSubtitleSelectable(true)
		overview.Add(r)
	}
	if t.Platform != "" {
		r := adw.NewActionRow()
		r.SetTitle("Platform")
		r.SetSubtitle(strings.TrimSpace(t.Platform))
		overview.Add(r)
	}
	if t.CategoriesText() != "" {
		r := adw.NewActionRow()
		r.SetTitle("Categories")
		r.SetSubtitle(t.CategoriesText())
		overview.Add(r)
	}
	if logo := strings.TrimSpace(t.Logo); logo != "" {
		r := adw.NewActionRow()
		r.SetTitle("Logo")
		r.SetSubtitle(logo)
		r.SetSubtitleSelectable(true)
		overview.Add(r)
	}
	page.Add(overview)

	// Stacks (type 2/3) carry their definition in a linked git repo. Surface
	// it so the dialog isn't empty for compose-style apps.
	if t.Repository != nil && (t.Repository.URL != "" || t.Repository.Stackfile != "") {
		srcGrp := adw.NewPreferencesGroup()
		srcGrp.SetTitle("Source")
		srcGrp.SetDescription("This app is deployed from a Compose stack hosted in a git repository.")
		if url := strings.TrimSpace(t.Repository.URL); url != "" {
			r := adw.NewActionRow()
			r.SetTitle("Repository")
			r.SetSubtitle(url)
			r.SetSubtitleSelectable(true)
			srcGrp.Add(r)
		}
		if sf := strings.TrimSpace(t.Repository.Stackfile); sf != "" {
			r := adw.NewActionRow()
			r.SetTitle("Stack file")
			r.SetSubtitle(sf)
			r.SetSubtitleSelectable(true)
			srcGrp.Add(r)
		}
		page.Add(srcGrp)
	}

	if note := strings.TrimSpace(t.Note); note != "" {
		notesGrp := adw.NewPreferencesGroup()
		notesGrp.SetTitle("Notes")

		card := gtk.NewBox(gtk.OrientationVertical, 0)
		card.AddCSSClass("card")
		card.SetMarginTop(4)
		card.SetHExpand(true)

		nb := gtk.NewLabel(noteToPlainText(note))
		nb.SetWrap(true)
		nb.SetWrapMode(pango.WrapWordChar)
		nb.SetXAlign(0)
		nb.SetYAlign(0)
		nb.SetSelectable(true)
		nb.SetHExpand(true)
		nb.SetMaxWidthChars(80)
		nb.SetMarginTop(12)
		nb.SetMarginBottom(12)
		nb.SetMarginStart(14)
		nb.SetMarginEnd(14)

		card.Append(nb)
		notesGrp.Add(card)
		page.Add(notesGrp)
	}

	envValues := map[string]string{}
	envEntries := map[string]*adw.EntryRow{}
	envCombos := map[string]*gtk.DropDown{}
	envSelectOptions := map[string][]apps.EnvSelectOpt{}
	if len(t.Env) > 0 {
		envGrp := adw.NewPreferencesGroup()
		envGrp.SetTitle("Configuration")
		envGrp.SetDescription("Environment variables passed to the container.")
		for _, e := range t.Env {
			e := e
			label := e.Label
			if label == "" {
				label = e.Name
			}
			if len(e.Select) > 0 {
				row := adw.NewActionRow()
				row.SetTitle(escapeMarkup(label))
				if e.Description != "" {
					row.SetSubtitle(escapeMarkup(e.Description))
				}
				options := make([]string, 0, len(e.Select))
				selectedIdx := uint(0)
				for i, opt := range e.Select {
					options = append(options, fmt.Sprintf("%s (%s)", opt.Text, opt.Value))
					if opt.Default {
						selectedIdx = uint(i)
					}
				}
				model := gtk.NewStringList(options)
				dd := gtk.NewDropDown(model, nil)
				dd.SetSelected(selectedIdx)
				dd.SetVAlign(gtk.AlignCenter)
				row.AddSuffix(dd)
				envGrp.Add(row)
				envCombos[e.Name] = dd
				envSelectOptions[e.Name] = e.Select
				envValues[e.Name] = e.Select[selectedIdx].Value
			} else {
				row := adw.NewEntryRow()
				row.SetTitle(escapeMarkup(label))
				row.SetText(e.Default)
				envGrp.Add(row)
				envEntries[e.Name] = row
				envValues[e.Name] = e.Default
			}
		}
		page.Add(envGrp)
	}

	if len(t.Ports) > 0 {
		g := adw.NewPreferencesGroup()
		g.SetTitle("Ports")
		for _, p := range t.Ports {
			r := adw.NewActionRow()
			r.SetTitle(p)
			g.Add(r)
		}
		page.Add(g)
	}
	if len(t.Volumes) > 0 {
		g := adw.NewPreferencesGroup()
		g.SetTitle("Volumes")
		g.SetDescription("Bind paths are mapped to /var/lib/orchestrator/apps/<slug>/ on each node.")
		for _, vol := range t.Volumes {
			r := adw.NewActionRow()
			r.SetTitle(coalesce(vol.Container, "(anonymous)"))
			if vol.Bind != "" {
				r.SetSubtitle(vol.Bind)
			}
			g.Add(r)
		}
		page.Add(g)
	}

	logGrp := adw.NewPreferencesGroup()
	logGrp.SetTitle("Status")
	logRow := adw.NewActionRow()
	logRow.SetTitle("Log")
	logRow.SetSubtitle("Idle.")
	logRow.SetSubtitleLines(0)
	logRow.SetSubtitleSelectable(true)
	logGrp.Add(logRow)
	page.Add(logGrp)

	scroll.SetChild(page)
	tv.SetContent(scroll)
	dialog.SetChild(tv)

	collectEnv := func() {
		for name, row := range envEntries {
			envValues[name] = row.Text()
		}
		for name, dd := range envCombos {
			opts := envSelectOptions[name]
			idx := int(dd.Selected())
			if idx >= 0 && idx < len(opts) {
				envValues[name] = opts[idx].Value
			}
		}
	}

	setLog := func(msg string, kind string) {
		stamp := time.Now().Format("15:04:05")
		current := logRow.Subtitle()
		if current == "Idle." {
			current = ""
		}
		next := fmt.Sprintf("%s [%s] %s", stamp, kind, msg)
		if current != "" {
			next = current + "\n" + next
		}
		logRow.SetSubtitle(escapeMarkup(next))
	}

	updateInstalledState := func() {
		_, installed := v.installedSlugs[t.Slug()]
		uninstallBtn.SetSensitive(installed)
		if installed {
			installBtn.SetLabel("Reinstall")
		} else {
			installBtn.SetLabel("Install")
		}
	}
	updateInstalledState()

	installBtn.ConnectClicked(func() {
		collectEnv()
		installBtn.SetSensitive(false)
		uninstallBtn.SetSensitive(false)
		setLog("translating template…", "info")
		go func() {
			m, err := apps.Translate(v.ctx, t, envValues, "")
			if err != nil {
				glib.IdleAdd(func() {
					installBtn.SetSensitive(true)
					updateInstalledState()
					setLog("translate failed: "+err.Error(), "error")
				})
				return
			}
			glib.IdleAdd(func() {
				setLog(fmt.Sprintf("applying %d objects to namespace %s", len(m.Objects), m.Namespace), "info")
			})
			err = apps.Apply(v.ctx, v.ClusterState.Cluster.Client, m)
			glib.IdleAdd(func() {
				installBtn.SetSensitive(true)
				if err != nil {
					setLog("apply failed: "+err.Error(), "error")
					updateInstalledState()
					return
				}
				setLog("install complete", "ok")
				v.installedSlugs[t.Slug()] = struct{}{}
				updateInstalledState()
				v.refreshInstalledFlags()
				ctxt.MustFrom[*adw.ToastOverlay](v.ctx).AddToast(
					adw.NewToast(fmt.Sprintf("Installed %s", t.DisplayName())))
			})
		}()
	})

	uninstallBtn.ConnectClicked(func() {
		confirm := adw.NewAlertDialog(
			fmt.Sprintf("Uninstall %s?", t.DisplayName()),
			fmt.Sprintf("This deletes namespace %s%s and all its workloads.", apps.NamespacePrefix, t.Slug()),
		)
		confirm.AddResponse("cancel", "Cancel")
		confirm.AddResponse("delete", "Uninstall")
		confirm.SetResponseAppearance("delete", adw.ResponseDestructive)
		confirm.ConnectResponse(func(response string) {
			if response != "delete" {
				return
			}
			installBtn.SetSensitive(false)
			uninstallBtn.SetSensitive(false)
			setLog("deleting namespace…", "info")
			go func() {
				err := apps.Uninstall(v.ctx, v.ClusterState.Cluster.Client, t.Slug())
				glib.IdleAdd(func() {
					installBtn.SetSensitive(true)
					if err != nil {
						setLog("uninstall failed: "+err.Error(), "error")
						updateInstalledState()
						return
					}
					delete(v.installedSlugs, t.Slug())
					setLog("uninstall complete", "ok")
					updateInstalledState()
					v.refreshInstalledFlags()
					ctxt.MustFrom[*adw.ToastOverlay](v.ctx).AddToast(
						adw.NewToast(fmt.Sprintf("Uninstalled %s", t.DisplayName())))
				})
			}()
		})
		confirm.Present(parent)
	})

	dialog.Present(parent)
}

func (v *AppsView) refreshInstalledFlags() {
	go func() {
		installed, err := apps.ListInstalledSlugs(v.ctx, v.ClusterState.Cluster.Client)
		if err != nil {
			return
		}
		glib.IdleAdd(func() {
			v.installedSlugs = map[string]struct{}{}
			for _, s := range installed {
				v.installedSlugs[s] = struct{}{}
			}
			if v.catalog != nil {
				v.renderTemplates()
			}
		})
	}()
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

var _ = stripHTML // kept for future plain-text fallbacks

// noteToPlainText turns the small subset of HTML that templates use in the
// note field into readable plain text: <br>/<p> become newlines, all other
// tags are dropped, and HTML entities are decoded.
func noteToPlainText(s string) string {
	s = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?i)</p>`).ReplaceAllString(s, "\n\n")
	s = regexp.MustCompile(`(?i)<p[^>]*>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, "")
	s = strings.NewReplacer(
		"&nbsp;", " ",
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&apos;", "'",
	).Replace(s)
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// noteToPangoMarkup converts the small subset of HTML that templates use in
// the note field (<br>, <b>, <i>, <code>, <a>) into Pango markup. Unknown tags
// are dropped and stray ampersands are escaped.
func noteToPangoMarkup(s string) string {
	// Normalise <br> variants to newlines first.
	brRe := regexp.MustCompile(`(?i)<br\s*/?>`)
	s = brRe.ReplaceAllString(s, "\n")
	// Drop <p> wrappers but keep paragraph spacing.
	pOpen := regexp.MustCompile(`(?i)<p[^>]*>`)
	s = pOpen.ReplaceAllString(s, "")
	pClose := regexp.MustCompile(`(?i)</p>`)
	s = pClose.ReplaceAllString(s, "\n\n")

	// Split on tags so we can decide which to keep.
	tagRe := regexp.MustCompile(`<[^>]+>`)
	var b strings.Builder
	last := 0
	keep := map[string]bool{
		"b": true, "/b": true, "i": true, "/i": true,
		"tt": true, "/tt": true, "code": true, "/code": true,
		"u": true, "/u": true,
	}
	for _, idx := range tagRe.FindAllStringIndex(s, -1) {
		// Escape text before the tag.
		b.WriteString(escapeMarkup(s[last:idx[0]]))
		tag := s[idx[0]+1 : idx[1]-1]
		lower := strings.ToLower(strings.SplitN(strings.TrimSpace(tag), " ", 2)[0])
		switch {
		case lower == "code" || lower == "/code":
			// Pango uses <tt> for monospace.
			if lower == "code" {
				b.WriteString("<tt>")
			} else {
				b.WriteString("</tt>")
			}
		case strings.HasPrefix(lower, "a") && (lower == "a" || strings.HasPrefix(lower, "a ")):
			// Best-effort: keep <a href="…"> as Pango supports it.
			b.WriteString("<")
			b.WriteString(tag)
			b.WriteString(">")
		case lower == "/a":
			b.WriteString("</a>")
		case keep[lower]:
			b.WriteString("<")
			b.WriteString(tag)
			b.WriteString(">")
		}
		last = idx[1]
	}
	b.WriteString(escapeMarkup(s[last:]))

	out := strings.TrimSpace(b.String())
	// Collapse runs of >2 newlines to exactly 2.
	out = regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n")
	return out
}

// keep pango import in use (may be needed later for ellipsizing custom rows)
var _ = pango.EllipsizeEnd
