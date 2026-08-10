// Adds a "cancelled" status to searches so a user can give up on a
// hung search from the UI instead of it sitting at "searching" forever.
package pb_migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		return setSearchStatusValues(app, []string{"queued", "searching", "complete", "failed", "cancelled"})
	}, func(app core.App) error {
		return setSearchStatusValues(app, []string{"queued", "searching", "complete", "failed"})
	})
}

func setSearchStatusValues(app core.App, values []string) error {
	searches, err := app.FindCollectionByNameOrId("searches")
	if err != nil {
		return err
	}
	field, ok := searches.Fields.GetByName("status").(*core.SelectField)
	if !ok {
		return fmt.Errorf("searches.status is not a select field")
	}
	field.Values = values
	return app.Save(searches)
}
