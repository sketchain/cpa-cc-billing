package sqlite

import (
	"database/sql"

	"cpa-super-billing/internal/billing"
)

// UpsertPrices saves several custom prices in one transaction: either all of
// them are stored or none is.
func (d *DB) UpsertPrices(prices []billing.CustomPrice) error {
	return d.transact(func(tx *sql.Tx) error {
		for _, price := range prices {
			if err := upsertPrice(tx.Exec, price); err != nil {
				return err
			}
		}
		return nil
	})
}
