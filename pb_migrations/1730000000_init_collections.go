// Initial schema: searches, search_results, downloads — all owned by
// a user from the default `users` auth collection.
package pb_migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	m.Register(func(app core.App) error {
		users, err := app.FindCollectionByNameOrId("users")
		if err != nil {
			return err
		}

		// searches
		searches := core.NewBaseCollection("searches")
		searches.ListRule = types.Pointer("@request.auth.id != '' && owner = @request.auth.id")
		searches.ViewRule = types.Pointer("@request.auth.id != '' && owner = @request.auth.id")
		// Create/update/delete from the API are restricted to the server (custom routes
		// run with superuser context for inserts; users can't manipulate rows directly).
		searches.CreateRule = nil
		searches.UpdateRule = nil
		searches.DeleteRule = nil
		searches.Fields.Add(
			&core.RelationField{
				Name:          "owner",
				Required:      true,
				CollectionId:  users.Id,
				CascadeDelete: true,
				MaxSelect:     1,
			},
			&core.TextField{Name: "query", Required: true, Max: 200},
			&core.SelectField{
				Name:      "status",
				Required:  true,
				MaxSelect: 1,
				Values:    []string{"queued", "searching", "complete", "failed"},
			},
			&core.NumberField{Name: "result_count", OnlyInt: true},
			&core.TextField{Name: "error", Max: 500},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		if err := app.Save(searches); err != nil {
			return err
		}

		// search_results
		results := core.NewBaseCollection("search_results")
		results.ListRule = types.Pointer("@request.auth.id != '' && search.owner = @request.auth.id")
		results.ViewRule = types.Pointer("@request.auth.id != '' && search.owner = @request.auth.id")
		results.CreateRule = nil
		results.UpdateRule = nil
		results.DeleteRule = nil
		results.Fields.Add(
			&core.RelationField{
				Name:          "search",
				Required:      true,
				CollectionId:  searches.Id,
				CascadeDelete: true,
				MaxSelect:     1,
			},
			&core.TextField{Name: "server", Max: 100},
			&core.TextField{Name: "author", Max: 500},
			&core.TextField{Name: "title", Max: 500},
			&core.TextField{Name: "format", Max: 20},
			&core.TextField{Name: "size", Max: 50},
			&core.TextField{Name: "full", Required: true, Max: 2000},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		if err := app.Save(results); err != nil {
			return err
		}

		// downloads
		downloads := core.NewBaseCollection("downloads")
		downloads.ListRule = types.Pointer("@request.auth.id != '' && owner = @request.auth.id")
		downloads.ViewRule = types.Pointer("@request.auth.id != '' && owner = @request.auth.id")
		downloads.CreateRule = nil
		downloads.UpdateRule = nil
		downloads.DeleteRule = nil
		downloads.Fields.Add(
			&core.RelationField{
				Name:          "owner",
				Required:      true,
				CollectionId:  users.Id,
				CascadeDelete: true,
				MaxSelect:     1,
			},
			&core.RelationField{
				Name:          "result",
				CollectionId:  results.Id,
				CascadeDelete: false,
				MaxSelect:     1,
			},
			&core.TextField{Name: "command", Required: true, Max: 2000},
			&core.SelectField{
				Name:      "status",
				Required:  true,
				MaxSelect: 1,
				Values:    []string{"queued", "downloading", "complete", "failed"},
			},
			&core.TextField{Name: "filename", Max: 500},
			&core.NumberField{Name: "size_bytes", OnlyInt: true},
			&core.FileField{
				Name:      "file",
				MaxSelect: 1,
				MaxSize:   500 * 1024 * 1024, // 500 MB cap; raise if you want
				Protected: true,              // requires auth to fetch
			},
			&core.TextField{Name: "error", Max: 500},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		if err := app.Save(downloads); err != nil {
			return err
		}

		return nil
	}, func(app core.App) error {
		for _, name := range []string{"downloads", "search_results", "searches"} {
			c, err := app.FindCollectionByNameOrId(name)
			if err != nil {
				continue
			}
			if err := app.Delete(c); err != nil {
				return err
			}
		}
		return nil
	})
}
