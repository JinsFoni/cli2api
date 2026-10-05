package app

import (
	"context"

	"github.com/caigee-cmd/cli2api/internal/control"
	"github.com/caigee-cmd/cli2api/internal/providers/qoder"
)

func EntryModelRegions(entry map[string]any) []string { return control.EntryModelRegions(entry) }

func (a *App) catalogSource() *control.CatalogSource {
	qoderClient := qoder.NewClient(a.Manager.Store())
	native := qoder.DisplayCatalog{Native: func(ctx context.Context, accountID string, refresh bool) ([]map[string]any, error) {
		entries, err := qoderClient.FetchModelsNativeRows(ctx, accountID)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			qoder.ApplyModelPricing(entry)
			qoder.ApplyModelContext(entry)
		}
		return entries, nil
	}}
	return &control.CatalogSource{Providers: a.Providers, WorkerModels: native.Models, Accounts: func() []control.CatalogAccount {
		var out []control.CatalogAccount
		for _, item := range a.Pool.Items() {
			out = append(out, control.CatalogAccount{ID: item.ID, Provider: item.Provider, Region: item.Region, Worker: true})
		}
		return out
	}}
}
func (a *App) FetchWorkerModelsFor(refresh bool, accountID string) ([]map[string]any, error) {
	return a.FetchWorkerModelsForMode(refresh, accountID, control.CatalogModeMerge)
}
func (a *App) FetchWorkerModelsForMode(refresh bool, accountID string, mode control.CatalogMode) ([]map[string]any, error) {
	return a.catalogSource().Fetch(refresh, accountID, mode)
}
func (a *App) fetchWorkerModels(refresh bool) []map[string]any {
	models, _ := a.FetchWorkerModelsFor(refresh, "")
	return models
}
