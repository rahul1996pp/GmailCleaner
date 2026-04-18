package main

import (
	"encoding/json"
	"os"
	"sync"
)

// ── Multi-account support ─────────────────────────────────────────────────────
// Stores multiple Gmail accounts. One is "active" at a time.
// Each account has its own credential + cache file.

const accountsFile = "accounts.json"

type Account struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Active   bool   `json:"active"`
}

type AccountsData struct {
	Accounts []Account `json:"accounts"`
}

var (
	accountsMu   sync.RWMutex
	accountsData *AccountsData
)

func accountsPath() string {
	return baseDir + "/" + accountsFile
}

func loadAccounts() *AccountsData {
	data, err := os.ReadFile(accountsPath())
	if err != nil {
		return &AccountsData{Accounts: []Account{}}
	}
	var ad AccountsData
	if err := json.Unmarshal(data, &ad); err != nil {
		return &AccountsData{Accounts: []Account{}}
	}
	return &ad
}

func saveAccounts(ad *AccountsData) {
	_ = os.MkdirAll(baseDir, 0700)
	data, _ := json.MarshalIndent(ad, "", "  ")
	_ = os.WriteFile(accountsPath(), data, 0644)
}

func initAccounts() {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	accountsData = loadAccounts()

	// Migrate: if we have old-style single creds in base dir, import them
	if len(accountsData.Accounts) == 0 {
		creds, err := loadCreds()
		if err == nil && creds.Email != "" {
			accountsData.Accounts = append(accountsData.Accounts, Account{
				Email:    creds.Email,
				Password: creds.Password,
				Active:   true,
			})
			saveAccounts(accountsData)
		}
	}

	// Activate the profile directory for whichever account is active.
	for _, a := range accountsData.Accounts {
		if a.Active {
			switchProfile(a.Email)
			// Ensure creds exist in the profile dir
			_ = saveCreds(a.Email, a.Password)
			_ = os.MkdirAll(dataDir, 0700)
			_ = os.WriteFile(loginFlag, []byte(a.Email), 0600)
			return
		}
	}
}

// addAccount adds a new account (does not set active).
func addAccount(email, password string) bool {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	for _, a := range accountsData.Accounts {
		if a.Email == email {
			return false // already exists
		}
	}
	accountsData.Accounts = append(accountsData.Accounts, Account{
		Email:    email,
		Password: password,
		Active:   false,
	})
	saveAccounts(accountsData)
	return true
}

// removeAccount removes an account by email.
func removeAccount(email string) {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	var remaining []Account
	for _, a := range accountsData.Accounts {
		if a.Email != email {
			remaining = append(remaining, a)
		}
	}
	accountsData.Accounts = remaining
	saveAccounts(accountsData)
}

// switchAccount sets one account as active, deactivates others, switches
// the profile directory, and reloads all profile-specific data.
func switchAccount(email string) bool {
	accountsMu.Lock()
	defer accountsMu.Unlock()
	found := false
	var password string
	for i := range accountsData.Accounts {
		if accountsData.Accounts[i].Email == email {
			accountsData.Accounts[i].Active = true
			password = accountsData.Accounts[i].Password
			found = true
		} else {
			accountsData.Accounts[i].Active = false
		}
	}
	if !found {
		return false
	}
	saveAccounts(accountsData)

	// Switch to per-user profile directory
	switchProfile(email)

	// Write creds into the profile dir so IMAP code finds them
	_ = saveCreds(email, password)
	_ = os.MkdirAll(dataDir, 0700)
	_ = os.WriteFile(loginFlag, []byte(email), 0600)

	// Reload profile-specific data
	reloadProfileData()
	return true
}

// getActiveAccount returns the active account email.
func getActiveAccount() string {
	accountsMu.RLock()
	defer accountsMu.RUnlock()
	for _, a := range accountsData.Accounts {
		if a.Active {
			return a.Email
		}
	}
	return ""
}

// getAccountList returns a sanitised list (no passwords).
func getAccountList() []map[string]any {
	accountsMu.RLock()
	defer accountsMu.RUnlock()
	list := make([]map[string]any, 0, len(accountsData.Accounts))
	for _, a := range accountsData.Accounts {
		list = append(list, map[string]any{
			"email":  a.Email,
			"active": a.Active,
		})
	}
	return list
}

// syncAccountFromLogin updates accounts data when login succeeds and
// switches to the user's profile directory.
func syncAccountFromLogin(email, password string) {
	accountsMu.Lock()
	found := false
	for i := range accountsData.Accounts {
		if accountsData.Accounts[i].Email == email {
			accountsData.Accounts[i].Password = password
			accountsData.Accounts[i].Active = true
			found = true
		} else {
			accountsData.Accounts[i].Active = false
		}
	}
	if !found {
		accountsData.Accounts = append(accountsData.Accounts, Account{
			Email:    email,
			Password: password,
			Active:   true,
		})
	}
	saveAccounts(accountsData)
	accountsMu.Unlock()

	// Switch to per-user profile directory
	switchProfile(email)
	_ = saveCreds(email, password)
	_ = os.MkdirAll(dataDir, 0700)
	_ = os.WriteFile(loginFlag, []byte(email), 0600)

	// Reload profile-specific data
	reloadProfileData()
}
