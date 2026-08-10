// Adds relevance/dedup fields to search_results so results can be ranked
// by how well they match the search query instead of sorted alphabetically.
package pb_migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		results, err := app.FindCollectionByNameOrId("search_results")
		if err != nil {
			return err
		}
		results.Fields.Add(
			&core.NumberField{Name: "relevance"},
			&core.BoolField{Name: "is_duplicate"},
		)
		return app.Save(results)
	}, func(app core.App) error {
		results, err := app.FindCollectionByNameOrId("search_results")
		if err != nil {
			return err
		}
		results.Fields.RemoveByName("relevance")
		results.Fields.RemoveByName("is_duplicate")
		return app.Save(results)
	})
}
