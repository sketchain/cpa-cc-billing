package sqlite

import "database/sql"

// SQL exposes the connection to the cpa-super-billing tables, which version
// themselves beside the billing schema.
func (d *DB) SQL() *sql.DB { return d.db }
