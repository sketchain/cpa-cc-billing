package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-super-billing/internal/billing"
)

func TestReferencePricesSQLiteRestartHashAndAtomicFailure(t *testing.T) {
	raw, err := os.ReadFile("../billing/testdata/models_dev_prices.json")
	if err != nil {
		t.Fatal(err)
	}
	download := func(context.Context) ([]byte, error) { return raw, nil }
	path := filepath.Join(t.TempDir(), "state.db")
	open := func(path string) (billing.Repository, error) { return Open(path, nil) }
	store := billing.NewStore(open, download)
	defer store.Close()
	cfg := billing.DefaultConfig()
	cfg.StateFile = path
	if err = store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	first, err := store.EnsureReferencePrices()
	if err != nil || !first.Usable {
		t.Fatal(first, err)
	}
	database, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var storedHash string
	var storedCount int
	if err := database.db.QueryRow("SELECT content_hash, model_count FROM reference_prices_metadata WHERE id = 1").Scan(&storedHash, &storedCount); err != nil {
		t.Fatal(err)
	}
	if storedHash == "" || storedHash != first.ContentHash || storedCount != first.ModelCount {
		t.Fatalf("reference price metadata not persisted: hash=%q count=%d", storedHash, storedCount)
	}
	// A matching hash cannot certify reference prices with missing model rows.
	if _, err := database.db.Exec("DELETE FROM reference_prices WHERE provider_id='openai' AND model_id='gpt-4o'"); err != nil {
		t.Fatal(err)
	}
	incomplete, err := database.LoadReferencePriceMetadata(context.Background())
	if err != nil || incomplete.Usable {
		t.Fatal("missing model rows accepted", incomplete, err)
	}
	repaired, err := store.RefreshReferencePrices()
	if err != nil || !repaired.Changed || !repaired.Metadata.Usable {
		t.Fatal("refresh did not repair missing model rows", repaired, err)
	}
	first = repaired.Metadata
	// If an unchanged download rewrites prices this trigger fails the operation.
	_, err = database.db.Exec(`CREATE TRIGGER forbid_reference_price_rewrite BEFORE DELETE ON reference_prices BEGIN SELECT RAISE(FAIL,'unexpected rewrite'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.RefreshReferencePrices()
	if err != nil || result.Changed || result.Metadata.Version != first.Version || result.Metadata.FetchedAt.Before(first.FetchedAt) {
		t.Fatal(result, err)
	}
	before := result.Metadata
	raw = append(append([]byte{}, raw...), byte(' '))
	if _, err = store.RefreshReferencePrices(); err == nil {
		t.Fatal("expected import failure")
	}
	after, err := database.LoadReferencePriceMetadata(context.Background())
	if err != nil || after.ContentHash != before.ContentHash || !after.FetchedAt.Equal(before.FetchedAt) {
		t.Fatal(after, err)
	}
	_, _ = database.db.Exec("DROP TRIGGER forbid_reference_price_rewrite")
	if _, err = store.RefreshReferencePrices(); err != nil {
		t.Fatal(err)
	}
	_, err = store.UpsertPrice(billing.CustomPrice{ModelID: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	restored := billing.NewStore(open, download)
	defer restored.Close()
	if err = restored.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if !restored.ReferencePriceMetadata().Usable {
		t.Fatal("reference prices not restored")
	}
	p, _, err := restored.ResolveModelPrice("gpt-4o", "", false)
	if err != nil || p.Source != billing.PriceSourceCustom || p.InputPer1M != 0 {
		t.Fatal(p, err)
	}
	if err = restored.DeletePrice("gpt-4o"); err != nil {
		t.Fatal(err)
	}
	p, _, err = restored.ResolveModelPrice("gpt-4o", "", false)
	if err != nil || p.Source != billing.PriceSourceReference {
		t.Fatal(p, err)
	}
	if !restored.ReferencePriceMetadata().Usable {
		t.Fatal("deletion removed reference prices")
	}
}

func BenchmarkReferencePricesPricing(b *testing.B) {
	path := filepath.Join(b.TempDir(), "reference-prices.db")
	d, err := Open(path, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	rules := make([]billing.ReferencePrice, 10000)
	for i := range rules {
		rules[i] = billing.ReferencePrice{ProviderID: "vendor", ModelID: fmt.Sprintf("model-%05d", i), PriceRates: &billing.PriceRates{InputPer1M: 1, OutputPer1M: 2}}
	}
	if err := d.SaveReferencePrices(context.Background(), billing.ReferencePriceMetadata{ContentHash: "benchmark", ModelCount: len(rules), Version: 1, FetchedAt: time.Now()}, rules); err != nil {
		b.Fatal(err)
	}
	s := billing.NewStore(func(path string) (billing.Repository, error) { return Open(path, nil) }, nil)
	defer s.Close()
	cfg := billing.DefaultConfig()
	cfg.StateFile = path
	if err := s.Configure(cfg); err != nil {
		b.Fatal(err)
	}
	model := "vendor/model-05000"
	if _, _, err := s.ResolveModelPrice(model, model, false); err != nil {
		b.Fatal(err)
	}
	b.Run("hot", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, _, err := s.ResolveModelPrice(model, model, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sqlite_without_cache", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := s.ModelPriceRows([]string{model}, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("display_1000_models", func(b *testing.B) {
		models := make([]string, 1000)
		for i := range models {
			models[i] = fmt.Sprintf("vendor/model-%05d", i)
		}
		b.ReportAllocs()
		for b.Loop() {
			rows, err := s.ModelPriceRows(models, false)
			if err != nil || len(rows) != len(models) {
				b.Fatalf("rows=%d err=%v", len(rows), err)
			}
		}
	})
}

func BenchmarkReferencePricesRefresh(b *testing.B) {
	models := make(map[string]any, 10000)
	for i := range 10000 {
		models[fmt.Sprintf("model-%05d", i)] = map[string]any{"cost": map[string]any{"input": 1, "output": 2}}
	}
	raw, err := json.Marshal(map[string]any{"providers": map[string]any{"vendor": map[string]any{"models": models}}})
	if err != nil {
		b.Fatal(err)
	}
	var changed atomic.Bool
	download := func(context.Context) ([]byte, error) {
		if changed.Load() {
			return append(append([]byte{}, raw...), ' '), nil
		}
		return raw, nil
	}
	s := billing.NewStore(func(path string) (billing.Repository, error) { return Open(path, nil) }, download)
	defer s.Close()
	cfg := billing.DefaultConfig()
	cfg.StateFile = filepath.Join(b.TempDir(), "reference-prices.db")
	if err := s.Configure(cfg); err != nil {
		b.Fatal(err)
	}
	if _, err := s.EnsureReferencePrices(); err != nil {
		b.Fatal(err)
	}
	for _, name := range []string{"changed", "same_hash"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if name == "changed" {
					changed.Store(!changed.Load())
				}
				if _, err := s.RefreshReferencePrices(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestReferencePricesQueriesMatchKeysAndPreservesIdentity(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	prices := []billing.ReferencePrice{
		{ProviderID: "first", ModelID: "gpt-x", PriceRates: &billing.PriceRates{InputPer1M: 1}},
		{ProviderID: "second", ModelID: "gpt-x", PriceRates: &billing.PriceRates{InputPer1M: 2}},
		{ProviderID: "third", ModelID: "Family/GPT-Y", PriceRates: &billing.PriceRates{InputPer1M: 3}},
		{ProviderID: "first", ModelID: "gpt-z"},
		{ProviderID: "first", ModelID: "literal*", PriceRates: &billing.PriceRates{}},
	}
	metadata := billing.ReferencePriceMetadata{ContentHash: "fixture", ModelCount: len(prices), Version: 1, FetchedAt: time.Now()}
	if err := database.SaveReferencePrices(ctx, metadata, prices); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := database.db.QueryRow("SELECT count(*) FROM reference_prices").Scan(&rows); err != nil || rows != len(prices) {
		t.Fatalf("reference model identities were merged or synthesized: rows=%d err=%v", rows, err)
	}
	var matchKey string
	if err := database.db.QueryRow("SELECT match_key FROM reference_prices WHERE provider_id='third'").Scan(&matchKey); err != nil || matchKey != "gpt-y" {
		t.Fatalf("stored match key=%q err=%v", matchKey, err)
	}
	var nodeID, parentID, unused int
	var queryPlan string
	if err := database.db.QueryRow("EXPLAIN QUERY PLAN SELECT rates_json FROM reference_prices WHERE match_key IN (?)", "gpt-x").Scan(&nodeID, &parentID, &unused, &queryPlan); err != nil || !strings.Contains(queryPlan, "USING INDEX reference_prices_match_key") {
		t.Fatalf("candidate query did not use match key index: %q err=%v", queryPlan, err)
	}
	for _, test := range []struct {
		name  string
		count int
	}{
		{"gpt-x", 2}, {"gpt-y", 1}, {"gpt-z", 1},
		{"gpt-*", 0}, {"literal*", 1}, {"literal-other", 0},
	} {
		rows, err := database.ReferencePriceCandidates(ctx, []string{test.name})
		if err != nil || len(rows) != test.count {
			t.Fatalf("lookup %q: rows=%+v err=%v", test.name, rows, err)
		}
	}
	results, err := database.SearchReferencePrices(ctx, "gpt-x", 10)
	if err != nil || len(results) != 2 || results[0].ProviderID == results[1].ProviderID {
		t.Fatalf("search lost provider identity: %+v, %v", results, err)
	}
	results, err = database.SearchReferencePrices(ctx, "gpt-z", 10)
	if err != nil || len(results) != 0 {
		t.Fatal("missing rates offered as a reference price", results, err)
	}
	if _, err := database.db.Exec("DELETE FROM reference_prices WHERE provider_id='first' AND model_id='gpt-x'"); err != nil {
		t.Fatal(err)
	}
	metadata, err = database.LoadReferencePriceMetadata(ctx)
	if err != nil || metadata.Usable {
		t.Fatal("partial reference prices accepted", metadata, err)
	}
	results, err = database.ReferencePriceCandidates(ctx, []string{"gpt-x"})
	if err != nil || len(results) != 1 || results[0].ProviderID != "second" {
		t.Fatal("deletion changed another provider price", results, err)
	}
}

func TestReferencePriceSearchNormalizesModelsAndRanksSourcesBeforeLimit(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	const model = "deepseek-v4-flash"
	prices := []billing.ReferencePrice{
		{ProviderID: "deepseek", ModelID: model, IsCanonical: true, PriceRates: &billing.PriceRates{InputPer1M: 1}},
		{ProviderID: "deepseek", ModelID: model + "-vision-exp", IsCanonical: true, PriceRates: &billing.PriceRates{InputPer1M: 2}},
		{ProviderID: "unpriced", ModelID: model},
	}
	for i := range 25 {
		prices = append(prices, billing.ReferencePrice{
			ProviderID: fmt.Sprintf("a-reseller-%02d", i), ModelID: "deepseek/" + model,
			PriceRates: &billing.PriceRates{InputPer1M: 3},
		})
	}
	metadata := billing.ReferencePriceMetadata{ContentHash: "search-fixture", Version: 1, ModelCount: len(prices), FetchedAt: time.Now()}
	if err := database.SaveReferencePrices(ctx, metadata, prices); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{model, "codex/" + model, " claude/" + model + "(xhigh) "} {
		results, err := database.SearchReferencePrices(ctx, query, 20)
		if err != nil || len(results) != 20 {
			t.Fatalf("search %q returned %d results: %v", query, len(results), err)
		}
		if results[0].ProviderID != "deepseek" || results[0].ModelID != model || !results[0].IsCanonical {
			t.Fatalf("search %q did not prioritize the canonical model: %+v", query, results[0])
		}
		candidates, err := database.ReferencePriceCandidates(ctx, []string{billing.ReferenceModelKey(billing.ModelWithoutThinkingSuffix(query))})
		if err != nil {
			t.Fatal(err)
		}
		matched, found := billing.MatchReferencePrice(query, candidates)
		if !found || matched.ProviderID != results[0].ProviderID || matched.ModelID != results[0].ModelID {
			t.Fatalf("search %q differs from automatic matching: %+v", query, matched)
		}
	}
	for _, test := range []struct {
		query    string
		provider string
		model    string
	}{
		{"a-reseller-24/deepseek/" + model, "a-reseller-24", "deepseek/" + model},
		{"a-reseller-24/", "a-reseller-24", "deepseek/" + model},
		{"deepseek-v4", "deepseek", model},
		{"codex/" + model + "-vision-exp(high)", "deepseek", model + "-vision-exp"},
	} {
		results, err := database.SearchReferencePrices(ctx, test.query, 1)
		if err != nil || len(results) != 1 || results[0].ProviderID != test.provider || results[0].ModelID != test.model {
			t.Fatalf("search %q = %+v, %v", test.query, results, err)
		}
	}
	for _, query := range []string{"", " ", "codex/deepseek-v4-missing(high)", "deepseek-v4-*"} {
		results, err := database.SearchReferencePrices(ctx, query, 20)
		if err != nil || len(results) != 0 {
			t.Fatalf("unmatched search %q = %+v, %v", query, results, err)
		}
	}
}
