package icon

import (
	_ "embed"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

const resourceIconPath = "/dev/skynomads/Seabird/icons"

//go:generate go run cmd/main.go
//go:generate glib-compile-resources --target=icon.gresource gresource.xml

//go:embed icon.gresource
var data []byte

func Register() error {
	res, err := gio.NewResourceFromData(glib.NewBytesWithGo(data))
	if err != nil {
		return err
	}
	gio.ResourcesRegister(res)
	if display := gdk.DisplayGetDefault(); display != nil {
		gtk.IconThemeGetForDisplay(display).AddResourcePath(resourceIconPath)
	}

	return nil
}
