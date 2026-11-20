// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type walletView struct {
	WalletID  string `json:"wallet_id"`
	Currency  string `json:"currency"`
	Status    string `json:"status"`
	Balance   int64  `json:"balance_minor"`
	Blocked   int64  `json:"blocked_minor"`
	Available int64  `json:"available_minor"`
}

type holdView struct {
	HoldID    string `json:"hold_id"`
	Amount    int64  `json:"amount_minor"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"created_at"`
}

type customerView struct {
	CustomerID string     `json:"customer_id"`
	Email      string     `json:"email"`
	Phone      string     `json:"phone,omitempty"`
	Wallet     walletView `json:"wallet"`
}

// errStatus is returned by helpers to carry an HTTP status.
type errStatus struct {
	code int
	msg  string
}

func (e errStatus) Error() string { return e.msg }

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func fail(w http.ResponseWriter, err error) {
	var es errStatus
	if errors.As(err, &es) {
		writeJSON(w, es.code, errBody(es.msg))
		return
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		writeJSON(w, http.StatusConflict, errBody("already exists"))
		return
	}
	writeJSON(w, http.StatusInternalServerError, errBody("internal error"))
}

func decode(r *http.Request, dst any) error {
	body := http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := json.NewDecoder(body).Decode(dst); err != nil {
		return errStatus{http.StatusBadRequest, "invalid JSON body"}
	}
	return nil
}

// querier is satisfied by *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func loadWallet(ctx context.Context, q querier, id string) (walletView, error) {
	var v walletView
	err := q.QueryRowContext(ctx, `
		SELECT w.wallet_id, w.currency, w.status, w.balance_minor,
		       COALESCE((SELECT SUM(amount_minor) FROM wallet_holds
		                 WHERE wallet_id = w.wallet_id AND status = 'active'), 0)
		FROM wallets w WHERE w.wallet_id = ?`, id).
		Scan(&v.WalletID, &v.Currency, &v.Status, &v.Balance, &v.Blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return v, errStatus{http.StatusNotFound, "wallet not found"}
	}
	v.Available = v.Balance - v.Blocked
	return v, err
}

func (a *app) getCustomer(w http.ResponseWriter, r *http.Request) {
	var c customerView
	var phone sql.NullString
	var walletID string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT customer_id, email, phone, wallet_id FROM customers WHERE customer_id = ?`,
		r.PathValue("id")).Scan(&c.CustomerID, &c.Email, &phone, &walletID)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, errStatus{http.StatusNotFound, "customer not found"})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	c.Phone = phone.String
	if c.Wallet, err = loadWallet(r.Context(), a.db, walletID); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (a *app) getWallet(w http.ResponseWriter, r *http.Request) {
	v, err := loadWallet(r.Context(), a.db, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT hold_id, amount_minor, COALESCE(reason, ''), created_at
		FROM wallet_holds WHERE wallet_id = ? AND status = 'active' ORDER BY created_at, hold_id`, v.WalletID)
	if err != nil {
		fail(w, err)
		return
	}
	defer rows.Close()
	holds := []holdView{}
	for rows.Next() {
		var h holdView
		if err := rows.Scan(&h.HoldID, &h.Amount, &h.Reason, &h.CreatedAt); err != nil {
			fail(w, err)
			return
		}
		holds = append(holds, h)
	}
	writeJSON(w, http.StatusOK, struct {
		walletView
		Holds []holdView `json:"holds"`
	}{v, holds})
}

func (a *app) createCustomer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CustomerID string `json:"customer_id"`
		Email      string `json:"email"`
		Phone      string `json:"phone"`
		Currency   string `json:"currency"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	if in.Email == "" {
		fail(w, errStatus{http.StatusBadRequest, "email is required"})
		return
	}
	if in.CustomerID == "" {
		in.CustomerID = newID("cus")
	}
	if in.Currency == "" {
		in.Currency = "EUR"
	}
	walletID, ts := newID("wal"), now()

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(),
		`INSERT INTO wallets (wallet_id, currency, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		walletID, in.Currency, ts, ts); err == nil {
		_, err = tx.ExecContext(r.Context(),
			`INSERT INTO customers (customer_id, email, phone, wallet_id, created_at) VALUES (?, ?, NULLIF(?, ''), ?, ?)`,
			in.CustomerID, in.Email, in.Phone, walletID, ts)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, err)
		return
	}
	r.SetPathValue("id", in.CustomerID)
	a.getCustomer(w, r)
}

// withWallet runs a balance-affecting write in one transaction against the wallet.
func (a *app) withWallet(w http.ResponseWriter, r *http.Request, fn func(tx *sql.Tx, v walletView) error) {
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	v, err := loadWallet(r.Context(), tx, r.PathValue("id"))
	if err == nil {
		err = fn(tx, v)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, err)
		return
	}
	a.getWallet(w, r)
}

func readAmount(r *http.Request) (int64, string, error) {
	var in struct {
		Amount int64  `json:"amount_minor"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		return 0, "", err
	}
	if in.Amount <= 0 {
		return 0, "", errStatus{http.StatusBadRequest, "amount_minor must be > 0"}
	}
	return in.Amount, in.Reason, nil
}

func (a *app) credit(w http.ResponseWriter, r *http.Request) {
	amt, _, err := readAmount(r)
	if err != nil {
		fail(w, err)
		return
	}
	a.withWallet(w, r, func(tx *sql.Tx, v walletView) error {
		if v.Status != "active" {
			return errStatus{http.StatusConflict, "wallet is " + v.Status}
		}
		_, err := tx.ExecContext(r.Context(),
			`UPDATE wallets SET balance_minor = balance_minor + ?, updated_at = ? WHERE wallet_id = ?`,
			amt, now(), v.WalletID)
		return err
	})
}

func (a *app) debit(w http.ResponseWriter, r *http.Request) {
	amt, _, err := readAmount(r)
	if err != nil {
		fail(w, err)
		return
	}
	a.withWallet(w, r, func(tx *sql.Tx, v walletView) error {
		if v.Status != "active" {
			return errStatus{http.StatusConflict, "wallet is " + v.Status}
		}
		if amt > v.Available {
			return errStatus{http.StatusConflict, "insufficient available funds"}
		}
		_, err := tx.ExecContext(r.Context(),
			`UPDATE wallets SET balance_minor = balance_minor - ?, updated_at = ? WHERE wallet_id = ?`,
			amt, now(), v.WalletID)
		return err
	})
}

func (a *app) createHold(w http.ResponseWriter, r *http.Request) {
	amt, reason, err := readAmount(r)
	if err != nil {
		fail(w, err)
		return
	}
	a.withWallet(w, r, func(tx *sql.Tx, v walletView) error {
		if amt > v.Available {
			return errStatus{http.StatusConflict, "insufficient available funds"}
		}
		_, err := tx.ExecContext(r.Context(),
			`INSERT INTO wallet_holds (hold_id, wallet_id, amount_minor, reason, created_at) VALUES (?, ?, ?, NULLIF(?, ''), ?)`,
			newID("hld"), v.WalletID, amt, reason, now())
		return err
	})
}

func (a *app) releaseHold(w http.ResponseWriter, r *http.Request) {
	var walletID string
	err := a.db.QueryRowContext(r.Context(),
		`SELECT wallet_id FROM wallet_holds WHERE hold_id = ?`, r.PathValue("id")).Scan(&walletID)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, errStatus{http.StatusNotFound, "hold not found"})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	res, err := a.db.ExecContext(r.Context(),
		`UPDATE wallet_holds SET status = 'released', released_at = ? WHERE hold_id = ? AND status = 'active'`,
		now(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, errStatus{http.StatusConflict, "hold already released"})
		return
	}
	r.SetPathValue("id", walletID)
	a.getWallet(w, r)
}

func (a *app) setStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, err)
		return
	}
	switch in.Status {
	case "active", "frozen", "closed":
	default:
		fail(w, errStatus{http.StatusBadRequest, "status must be active, frozen or closed"})
		return
	}
	a.withWallet(w, r, func(tx *sql.Tx, v walletView) error {
		_, err := tx.ExecContext(r.Context(),
			`UPDATE wallets SET status = ?, updated_at = ? WHERE wallet_id = ?`, in.Status, now(), v.WalletID)
		return err
	})
}
