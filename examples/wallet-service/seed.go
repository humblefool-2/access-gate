// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"fmt"
)

// seedDemo inserts a few customers and wallets when the table is empty.
// No holds are created, so there is nothing blocked or pending.
func seedDemo(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM customers`).Scan(&n); err != nil || n > 0 {
		return err
	}
	demo := []struct {
		id, email, phone, status string
		balance                  int64
	}{
		{"cus_demo1", "ana@example.com", "+40700000001", "active", 125000},
		{"cus_demo2", "ben@example.com", "+40700000002", "active", 4500},
		{"cus_demo3", "chloe@example.com", "+40700000003", "active", 0},
		{"cus_demo4", "dan@example.com", "+40700000004", "frozen", 98000},
		{"cus_demo5", "eva@example.com", "+40700000005", "active", 250},
	}
	ts := now()
	for i, d := range demo {
		wid := fmt.Sprintf("wal_demo%d", i+1)
		if _, err := db.Exec(`INSERT INTO wallets (wallet_id, balance_minor, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			wid, d.balance, d.status, ts, ts); err != nil {
			return err
		}
		if _, err := db.Exec(`INSERT INTO customers (customer_id, email, phone, wallet_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			d.id, d.email, d.phone, wid, ts); err != nil {
			return err
		}
	}
	return nil
}
