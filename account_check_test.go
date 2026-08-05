package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestCheckAccountRejectsAnotherUserBeforeConnecting(t *testing.T) {
	t.Setenv(mailKeyEnv, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	vault, err := loadCredentialVault()
	if err != nil {
		t.Fatal(err)
	}
	sqldb := openTestMailDB(t)
	defer sqldb.Close()
	ciphertext, nonce, err := vault.Seal(accountCredential{AppPassword: "not-a-real-password"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := sqldb.Exec(`INSERT INTO accounts
		(login, provider, email, credential_ciphertext, credential_nonce)
		VALUES ('owner', 'icloud', 'owner@icloud.com', ?, ?)`, ciphertext, nonce)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	syncer := &mailSynchronizer{db: sqldb, vault: vault}
	_, err = syncer.CheckAccount(context.Background(), "intruder", accountID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("CheckAccount() error = %v, want sql.ErrNoRows", err)
	}
}
