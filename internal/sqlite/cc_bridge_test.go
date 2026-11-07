package sqlite

import (
	"testing"

	"cpa-super-billing/internal/billing"
)

func TestUpsertPricesRollsBackTogether(t *testing.T) {
	d := openTestDB(t)
	valid := billing.CustomPrice{ModelID: "import-ok", Currency: billing.USD, PriceRates: billing.PriceRates{InputPer1M: 1, OutputPer1M: 2}}
	invalid := billing.CustomPrice{ModelID: "import-bad", Currency: billing.USD, PriceRates: billing.PriceRates{InputPer1M: -1, OutputPer1M: 2}}
	if err := d.UpsertPrices([]billing.CustomPrice{valid, invalid}); err == nil {
		t.Fatal("an import with an invalid price succeeded")
	}
	var count int
	if err := d.db.QueryRow(`SELECT count(*) FROM prices WHERE model_id IN ('import-ok', 'import-bad')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("%d prices survived the failed import", count)
	}
	if err := d.UpsertPrices([]billing.CustomPrice{valid}); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRow(`SELECT count(*) FROM prices WHERE model_id = 'import-ok'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored prices = %d, %v", count, err)
	}
}
