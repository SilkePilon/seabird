package ui

import (
	"fmt"
	"time"

	"github.com/SilkePilon/Orchestrator/internal/apps"
	"github.com/SilkePilon/Orchestrator/internal/ctxt"
	"github.com/SilkePilon/Orchestrator/internal/util"
	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4-sourceview/pkg/gtksource/v5"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// openImportDialog presents a dialog that lets the user paste a compose file,
// a Kubernetes manifest, or a Portainer-style container template and install
// it as a custom app.
func (v *AppsView) openImportDialog() {
	parent := ctxt.MustFrom[*gtk.Window](v.ctx)

	dialog := adw.NewDialog()
	dialog.SetTitle("Import custom app")
	dialog.SetContentWidth(760)
	dialog.SetContentHeight(640)

	tv := adw.NewToolbarView()
	tv.AddCSSClass("view")
	header := adw.NewHeaderBar()
	header.SetTitleWidget(adw.NewWindowTitle("Import custom app", "Paste a compose file, manifest or container JSON"))
	tv.AddTopBar(header)

	installBtn := gtk.NewButtonWithLabel("Install")
	installBtn.AddCSSClass("suggested-action")
	header.PackEnd(installBtn)

	body := gtk.NewBox(gtk.OrientationVertical, 12)
	body.SetMarginTop(12)
	body.SetMarginBottom(12)
	body.SetMarginStart(12)
	body.SetMarginEnd(12)

	formGrp := adw.NewPreferencesGroup()

	nameRow := adw.NewEntryRow()
	nameRow.SetTitle("Name")
	formGrp.Add(nameRow)

	kindModel := gtk.NewStringList([]string{
		"Docker Compose (YAML or JSON)",
		"Kubernetes manifest (YAML)",
		"Single container (Portainer JSON)",
	})
	kindRow := adw.NewComboRow()
	kindRow.SetTitle("Kind")
	kindRow.SetModel(kindModel)
	formGrp.Add(kindRow)

	body.Append(formGrp)

	editorLabel := gtk.NewLabel("Definition")
	editorLabel.SetXAlign(0)
	editorLabel.AddCSSClass("heading")
	body.Append(editorLabel)

	buf := gtksource.NewBufferWithLanguage(gtksource.LanguageManagerGetDefault().Language("yaml"))
	util.SetSourceColorScheme(buf)
	source := gtksource.NewViewWithBuffer(buf)
	source.SetMonospace(true)
	source.SetVExpand(true)
	source.SetShowLineNumbers(true)
	source.SetMarginStart(4)
	source.SetMarginEnd(4)
	source.SetMarginTop(4)
	source.SetMarginBottom(4)

	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetHExpand(true)
	scroll.SetChild(source)
	scroll.AddCSSClass("card")
	body.Append(scroll)

	logRow := adw.NewActionRow()
	logRow.SetTitle("Log")
	logRow.SetSubtitle("Idle.")
	logRow.SetSubtitleLines(0)
	logRow.SetSubtitleSelectable(true)
	logGrp := adw.NewPreferencesGroup()
	logGrp.Add(logRow)
	body.Append(logGrp)

	tv.SetContent(body)
	dialog.SetChild(tv)

	// Seed the buffer with a hint matching the initially-selected kind.
	setPlaceholder := func(idx uint) {
		if buf.Text(buf.StartIter(), buf.EndIter(), false) != "" {
			return
		}
		switch idx {
		case 0:
			buf.SetText("services:\n  web:\n    image: nginx:latest\n    ports:\n      - \"8080:80\"\n")
		case 1:
			buf.SetText("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: example\nspec:\n  replicas: 1\n  selector:\n    matchLabels:\n      app: example\n  template:\n    metadata:\n      labels:\n        app: example\n    spec:\n      containers:\n        - name: example\n          image: nginx:latest\n")
		case 2:
			buf.SetText("{\n  \"image\": \"nginx:latest\",\n  \"ports\": [\"8080:80/tcp\"],\n  \"env\": [\n    { \"name\": \"EXAMPLE\", \"label\": \"Example\", \"default\": \"value\" }\n  ]\n}\n")
		}
	}
	setPlaceholder(kindRow.Selected())
	kindRow.Connect("notify::selected", func() { setPlaceholder(kindRow.Selected()) })

	setLog := func(msg, kind string) {
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

	installBtn.ConnectClicked(func() {
		slug := apps.SanitizeSlug(nameRow.Text())
		if slug == "" {
			setLog("a name is required", "error")
			return
		}
		raw := []byte(buf.Text(buf.StartIter(), buf.EndIter(), true))
		var kind apps.CustomKind
		switch kindRow.Selected() {
		case 0:
			kind = apps.CustomKindCompose
		case 1:
			kind = apps.CustomKindManifest
		case 2:
			kind = apps.CustomKindContainer
		}

		installBtn.SetSensitive(false)
		setLog("translating…", "info")
		go func() {
			m, err := apps.TranslateCustom(v.ctx, kind, raw, slug, nil, "")
			if err != nil {
				glib.IdleAdd(func() {
					installBtn.SetSensitive(true)
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
					return
				}
				setLog("install complete", "ok")
				v.installedSlugs[slug] = struct{}{}
				v.refreshInstalledFlags()
				ctxt.MustFrom[*adw.ToastOverlay](v.ctx).AddToast(
					adw.NewToast(fmt.Sprintf("Installed %s", slug)))
				dialog.Close()
			})
		}()
	})

	dialog.Present(parent)
}
