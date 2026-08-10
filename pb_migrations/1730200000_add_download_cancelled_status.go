// Adds a "cancelled" status to downloads so a user can give up on a
// hung download from the UI instead of it sitting stuck forever.
package pb_migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		return setDownloadStatusValues(app, []string{"queued", "downloading", "complete", "failed", "cancelled"})
	}, func(app core.App) error {
		return setDownloadStatusValues(app, []string{"queued", "downloading", "complete", "failed"})
	})
}

func setDownloadStatusValues(app core.App, values []string) error {
	downloads, err := app.FindCollectionByNameOrId("downloads")
	if err != nil {
		return err
	}
	field, ok := downloads.Fields.GetByName("status").(*core.SelectField)
	if !ok {
		return fmt.Errorf("downloads.status is not a select field")
	}
	field.Values = values
	return app.Save(downloads)
}
