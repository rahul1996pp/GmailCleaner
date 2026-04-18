package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// ── Shared state ──────────────────────────────────────────────────────────────

var (
	mu          sync.RWMutex
	loginStatus = statusMsg{Status: "idle", Message: "Not started"}
	fetchStatus = fetchMsg{Status: "idle", Message: "Not started", Progress: 0}
	emailCache  *CategorizeResult
)

type statusMsg struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type fetchMsg struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Progress int    `json:"progress"`
}

// ── Background workers ────────────────────────────────────────────────────────

// reloadProfileData reloads all profile-specific data from disk (tags,
// delete history, email cache) after switching accounts/profiles.
func reloadProfileData() {
	initTagData()
	initDeleteHistory()
	if cached, _ := loadEmailCache(); len(cached) > 0 {
		result := rebuildFromCached(cached)
		mu.Lock()
		emailCache = &result
		mu.Unlock()
	} else {
		mu.Lock()
		emailCache = nil
		mu.Unlock()
	}
}

func doLogin(email, password string) {
	status, message := loginSync(email, password)
	mu.Lock()
	loginStatus = statusMsg{Status: status, Message: message}
	mu.Unlock()
	if status == "logged_in" {
		syncAccountFromLogin(email, password)
	}
}

func doFetch() {
	mu.Lock()
	fetchStatus = fetchMsg{Status: "running", Message: "Syncing new emails from IMAP...", Progress: 20}
	mu.Unlock()

	rawEmails, err := fetchEmailsSync(false)
	if err != nil {
		mu.Lock()
		fetchStatus = fetchMsg{Status: "error", Message: err.Error(), Progress: 0}
		mu.Unlock()
		return
	}

	mu.Lock()
	fetchStatus = fetchMsg{Status: "running", Message: "Categorizing emails...", Progress: 80}
	mu.Unlock()

	result := categorizeEmails(rawEmails)

	// Apply user category overrides
	applyCategoryOverrides(result.Emails)
	result = rebuildFromCached(result.Emails)

	mu.Lock()
	emailCache = &result
	fetchStatus = fetchMsg{
		Status:   "done",
		Message:  fmt.Sprintf("Loaded %d emails", len(rawEmails)),
		Progress: 100,
	}
	mu.Unlock()
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"detail":"marshal error"}`, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeErr(w http.ResponseWriter, code int, detail string) {
	writeJSON(w, code, map[string]string{"detail": detail})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func serveStaticFile(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(data)
}

// ── Route handlers ────────────────────────────────────────────────────────────

func handleStatus(w http.ResponseWriter, _ *http.Request) {
	loggedIn := isLoggedIn()
	mu.RLock()
	ls := loginStatus
	fs := fetchStatus
	hasCached := emailCache != nil
	mu.RUnlock()

	writeJSON(w, 200, map[string]any{
		"logged_in":      loggedIn,
		"login_task":     ls,
		"fetch_task":     fs,
		"cached_emails":  hasCached,
		"active_account": getActiveAccount(),
		"accounts":       getAccountList(),
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil || body.Email == "" || body.Password == "" {
		writeErr(w, 400, "email and password are required")
		return
	}
	mu.Lock()
	loginStatus = statusMsg{Status: "verifying", Message: "Verifying credentials..."}
	mu.Unlock()

	go doLogin(body.Email, body.Password)
	writeJSON(w, 200, map[string]string{"message": "Verifying credentials..."})
}

func handleLogout(w http.ResponseWriter, _ *http.Request) {
	// Only remove the login flag — keep credentials in accounts.json
	// so the user can auto-login later.
	_ = os.Remove(loginFlag)
	mu.Lock()
	emailCache = nil
	loginStatus = statusMsg{Status: "idle", Message: "Logged out"}
	mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"message":  "Logged out successfully",
		"accounts": getAccountList(),
	})
}

func handleEmails(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "true"

	if !refresh {
		mu.RLock()
		cache := emailCache
		mu.RUnlock()
		if cache != nil {
			writeJSON(w, 200, map[string]any{
				"emails":            cache.Emails,
				"stats":             cache.Stats,
				"total":             len(cache.Emails),
				"tags":              getTagMap(),
				"tag_summary":       getTagSummary(),
				"all_tags":          getAllTags(),
				"custom_categories": getCustomCategories(),
			})
		} else {
			writeJSON(w, 200, map[string]string{
				"status":  "no_cache",
				"message": `No local data. Press "Fetch Emails" to download.`,
			})
		}
		return
	}

	if !isLoggedIn() {
		writeErr(w, 401, "Not logged in to Gmail")
		return
	}

	mu.Lock()
	if fetchStatus.Status == "running" {
		mu.Unlock()
		writeJSON(w, 200, map[string]string{"status": "running", "message": "Already fetching..."})
		return
	}
	fetchStatus = fetchMsg{Status: "running", Message: "Connecting to IMAP...", Progress: 0}
	mu.Unlock()

	go doFetch()
	writeJSON(w, 200, map[string]string{"status": "running", "message": "Fetch started..."})
}

func handleFetchStatus(w http.ResponseWriter, _ *http.Request) {
	mu.RLock()
	fs := fetchStatus
	mu.RUnlock()
	writeJSON(w, 200, fs)
}

func handleEmailBody(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, 400, "id parameter required")
		return
	}
	if !isLoggedIn() {
		writeErr(w, 401, "Not logged in to Gmail")
		return
	}
	result := fetchEmailBodySync(id)
	if result.Error != "" {
		writeErr(w, 500, result.Error)
		return
	}
	writeJSON(w, 200, result)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ThreadIDs []string `json:"thread_ids"`
	}
	if err := readJSON(r, &body); err != nil || len(body.ThreadIDs) == 0 {
		writeErr(w, 400, "no thread IDs provided")
		return
	}
	if !isLoggedIn() {
		writeErr(w, 401, "Not logged in to Gmail")
		return
	}

	// Filter out protected emails (important/keep tagged)
	allowed, blocked := filterProtected(body.ThreadIDs)
	if len(allowed) == 0 {
		writeJSON(w, 200, map[string]any{
			"deleted": 0,
			"blocked": len(blocked),
			"errors":  []string{},
			"message": "All selected emails are tagged as important/keep and were protected from deletion.",
		})
		return
	}

	result := deleteEmailsSync(allowed)

	// Remove deleted emails from the in-memory cache and track deletions.
	if result.Deleted > 0 {
		deletedSet := make(map[string]bool, len(allowed))
		for _, id := range allowed {
			deletedSet[id] = true
		}
		mu.Lock()
		if emailCache != nil {
			// Collect deleted emails for pattern tracking
			var deletedEmails []Email
			remaining := make([]Email, 0, len(emailCache.Emails))
			for _, e := range emailCache.Emails {
				if deletedSet[e.ID] {
					deletedEmails = append(deletedEmails, e)
				} else {
					remaining = append(remaining, e)
				}
			}
			rebuilt := rebuildFromCached(remaining)
			emailCache = &rebuilt

			// Track deletion patterns and retrain AI (async)
			if len(deletedEmails) > 0 {
				go func(del []Email, all []Email) {
					recordDeletions(del)
					retrainOnDelete(del, all)
				}(deletedEmails, remaining)
			}
		}
		mu.Unlock()
	}

	resp := map[string]any{
		"deleted":     result.Deleted,
		"deleted_ids": allowed,
		"errors":      result.Errors,
	}
	if len(blocked) > 0 {
		resp["blocked"] = len(blocked)
		resp["message"] = sitoa(len(blocked)) + " email(s) protected by important/keep tag"
	}
	writeJSON(w, 200, resp)
}

func handleStats(w http.ResponseWriter, _ *http.Request) {
	mu.RLock()
	cache := emailCache
	mu.RUnlock()
	if cache == nil {
		writeJSON(w, 200, map[string]string{"message": "No data yet. Fetch emails first."})
		return
	}
	writeJSON(w, 200, cache.Stats)
}

func handleAsk(w http.ResponseWriter, r *http.Request) {
	var body AskRequest
	if err := readJSON(r, &body); err != nil || body.Question == "" {
		writeErr(w, 400, "question is required")
		return
	}

	mu.RLock()
	cache := emailCache
	mu.RUnlock()
	if cache == nil {
		writeJSON(w, 200, AskResponse{Answer: "No emails loaded yet. Please fetch emails first."})
		return
	}

	result := askAI(body.Question, cache.Emails, cache.Stats)
	writeJSON(w, 200, result)
}

func handleSuggestions(w http.ResponseWriter, _ *http.Request) {
	mu.RLock()
	cache := emailCache
	mu.RUnlock()
	if cache == nil {
		writeJSON(w, 200, map[string]any{"suggestions": []any{}, "message": "No data yet."})
		return
	}

	suggestions := computeDeleteSuggestions(cache.Emails)
	writeJSON(w, 200, map[string]any{
		"suggestions": suggestions,
		"total":       len(suggestions),
	})
}

func handleLabelDomainGroups(w http.ResponseWriter, _ *http.Request) {
	mu.RLock()
	cache := emailCache
	mu.RUnlock()
	if cache == nil {
		writeJSON(w, 200, map[string]any{"groups": []any{}})
		return
	}

	groups := buildLabelDomainGroups(cache.Emails)
	writeJSON(w, 200, map[string]any{
		"groups": groups,
		"total":  len(groups),
	})
}

func handleDeleteHistory(w http.ResponseWriter, _ *http.Request) {
	deleteHistMu.RLock()
	hist := deleteHist
	deleteHistMu.RUnlock()
	if hist == nil {
		writeJSON(w, 200, map[string]any{"total": 0, "patterns": map[string]any{}})
		return
	}

	domCounts := map[string]int{}
	catCounts := map[string]int{}
	senderCounts := map[string]int{}
	for _, r := range hist.Records {
		domCounts[r.Domain]++
		catCounts[r.Category]++
		senderCounts[r.Sender]++
	}

	writeJSON(w, 200, map[string]any{
		"total": len(hist.Records),
		"patterns": map[string]any{
			"by_domain":   domCounts,
			"by_category": catCounts,
			"by_sender":   senderCounts,
		},
	})
}

func handleEmptyTrash(w http.ResponseWriter, _ *http.Request) {
	if !isLoggedIn() {
		writeErr(w, 401, "Not logged in to Gmail")
		return
	}
	_, err := emptyTrashSync()
	if err != nil {
		writeErr(w, 500, "Failed to empty trash: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"message": "Trash emptied successfully"})
}

// ── Tag handlers ──────────────────────────────────────────────────────────────

func handleGetTags(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"tags":        getTagMap(),
		"all_tags":    getAllTags(),
		"tag_summary": getTagSummary(),
	})
}

func handleSetTags(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EmailIDs []string `json:"email_ids"`
		Tag      string   `json:"tag"`
		Action   string   `json:"action"` // "add" or "remove"
	}
	if err := readJSON(r, &body); err != nil || body.Tag == "" || len(body.EmailIDs) == 0 {
		writeErr(w, 400, "email_ids and tag are required")
		return
	}
	if body.Action == "remove" {
		removeTag(body.EmailIDs, body.Tag)
	} else {
		addTag(body.EmailIDs, body.Tag)
	}
	writeJSON(w, 200, map[string]any{
		"message":     "Tags updated",
		"tag_summary": getTagSummary(),
		"tags":        getTagMap(),
	})
}

func handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil || body.Name == "" {
		writeErr(w, 400, "tag name is required")
		return
	}
	writeJSON(w, 200, map[string]any{
		"message":  "Tag created",
		"all_tags": getAllTags(),
	})
}

// ── Custom category handlers ─────────────────────────────────────────────────

func handleGetCategories(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"categories":        getAllCategories(),
		"custom_categories": getCustomCategories(),
	})
}

func handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &body); err != nil || body.Name == "" {
		writeErr(w, 400, "category name is required")
		return
	}
	if !addCustomCategory(body.Name) {
		writeErr(w, 400, "category already exists")
		return
	}
	writeJSON(w, 200, map[string]any{
		"message":           "Category created",
		"categories":        getAllCategories(),
		"custom_categories": getCustomCategories(),
	})
}

func handleMoveCategory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EmailIDs []string `json:"email_ids"`
		Category string   `json:"category"`
	}
	if err := readJSON(r, &body); err != nil || body.Category == "" || len(body.EmailIDs) == 0 {
		writeErr(w, 400, "email_ids and category are required")
		return
	}
	moveToCategory(body.EmailIDs, body.Category)

	// Update in-memory cache
	mu.Lock()
	if emailCache != nil {
		overrides := getCategoryOverrides()
		for i := range emailCache.Emails {
			if cat, ok := overrides[emailCache.Emails[i].ID]; ok {
				emailCache.Emails[i].Category = cat
			}
		}
		rebuilt := rebuildFromCached(emailCache.Emails)
		emailCache = &rebuilt
	}
	mu.Unlock()

	writeJSON(w, 200, map[string]string{"message": "Emails moved to " + body.Category})
}

// ── Account handlers ──────────────────────────────────────────────────────────

func handleGetAccounts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{
		"accounts": getAccountList(),
	})
}

func handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil || body.Email == "" || body.Password == "" {
		writeErr(w, 400, "email and password are required")
		return
	}
	// Verify credentials without changing active profile
	if err := verifyIMAPCreds(body.Email, body.Password); err != nil {
		writeErr(w, 400, "Authentication failed: "+err.Error())
		return
	}
	if !addAccount(body.Email, body.Password) {
		writeErr(w, 400, "Account already exists")
		return
	}
	writeJSON(w, 200, map[string]any{
		"message":  "Account added",
		"accounts": getAccountList(),
	})
}

func handleSwitchAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil || body.Email == "" {
		writeErr(w, 400, "email is required")
		return
	}
	if !switchAccount(body.Email) {
		writeErr(w, 400, "account not found")
		return
	}
	// switchAccount already calls reloadProfileData which sets emailCache
	writeJSON(w, 200, map[string]any{
		"message":  "Switched to " + body.Email,
		"accounts": getAccountList(),
	})
}

func handleRemoveAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil || body.Email == "" {
		writeErr(w, 400, "email is required")
		return
	}
	active := getActiveAccount()
	if body.Email == active && isLoggedIn() {
		writeErr(w, 400, "cannot remove active account while logged in — logout first")
		return
	}
	removeAccount(body.Email)
	writeJSON(w, 200, map[string]any{
		"message":  "Account removed",
		"accounts": getAccountList(),
	})
}

// handleAutoLogin logs in using saved credentials from accounts.json.
func handleAutoLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil || body.Email == "" {
		writeErr(w, 400, "email is required")
		return
	}

	// Look up saved password
	accountsMu.RLock()
	var password string
	found := false
	for _, a := range accountsData.Accounts {
		if a.Email == body.Email {
			password = a.Password
			found = true
			break
		}
	}
	accountsMu.RUnlock()

	if !found {
		writeErr(w, 404, "account not found")
		return
	}

	mu.Lock()
	loginStatus = statusMsg{Status: "verifying", Message: "Auto-logging in as " + body.Email + "..."}
	mu.Unlock()

	go doLogin(body.Email, password)
	writeJSON(w, 200, map[string]string{"message": "Auto-logging in..."})
}

// ── Router ────────────────────────────────────────────────────────────────────

func newMux(staticDir string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// CORS pre-flight
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(204)
			return
		}

		path := r.URL.Path

		switch {
		case (path == "/" || path == "") && r.Method == http.MethodGet:
			serveStaticFile(w, filepath.Join(staticDir, "index.html"))

		case path == "/api/status" && r.Method == http.MethodGet:
			handleStatus(w, r)
		case path == "/api/login" && r.Method == http.MethodPost:
			handleLogin(w, r)
		case path == "/api/logout" && r.Method == http.MethodPost:
			handleLogout(w, r)
		case path == "/api/emails" && r.Method == http.MethodGet:
			handleEmails(w, r)
		case path == "/api/emails/status" && r.Method == http.MethodGet:
			handleFetchStatus(w, r)
		case path == "/api/email_body" && r.Method == http.MethodGet:
			handleEmailBody(w, r)
		case path == "/api/delete" && r.Method == http.MethodPost:
			handleDelete(w, r)
		case path == "/api/stats" && r.Method == http.MethodGet:
			handleStats(w, r)
		case path == "/api/ask" && r.Method == http.MethodPost:
			handleAsk(w, r)
		case path == "/api/suggestions" && r.Method == http.MethodGet:
			handleSuggestions(w, r)
		case path == "/api/label_domain_groups" && r.Method == http.MethodGet:
			handleLabelDomainGroups(w, r)
		case path == "/api/delete_history" && r.Method == http.MethodGet:
			handleDeleteHistory(w, r)
		case path == "/api/empty_trash" && r.Method == http.MethodPost:
			handleEmptyTrash(w, r)

		// Tags
		case path == "/api/tags" && r.Method == http.MethodGet:
			handleGetTags(w, r)
		case path == "/api/tags" && r.Method == http.MethodPost:
			handleSetTags(w, r)
		case path == "/api/tags/create" && r.Method == http.MethodPost:
			handleCreateTag(w, r)

		// Custom categories
		case path == "/api/categories" && r.Method == http.MethodGet:
			handleGetCategories(w, r)
		case path == "/api/categories/create" && r.Method == http.MethodPost:
			handleCreateCategory(w, r)
		case path == "/api/categories/move" && r.Method == http.MethodPost:
			handleMoveCategory(w, r)

		// Accounts
		case path == "/api/accounts" && r.Method == http.MethodGet:
			handleGetAccounts(w, r)
		case path == "/api/accounts/add" && r.Method == http.MethodPost:
			handleAddAccount(w, r)
		case path == "/api/accounts/switch" && r.Method == http.MethodPost:
			handleSwitchAccount(w, r)
		case path == "/api/accounts/remove" && r.Method == http.MethodPost:
			handleRemoveAccount(w, r)
		case path == "/api/accounts/autologin" && r.Method == http.MethodPost:
			handleAutoLogin(w, r)

		default:
			// Serve static assets, preventing directory traversal.
			fp := filepath.Join(staticDir, filepath.Clean("/"+path))
			if info, err := os.Stat(fp); err == nil && !info.IsDir() {
				serveStaticFile(w, fp)
			} else {
				writeErr(w, 404, "not found")
			}
		}
	})
	return mux
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	initPaths()    // set baseDir, dataDir, loginFlag, credsFile, cacheFile
	initAccounts() // loads accounts.json, switches profile for active account
	initDeleteHistory()
	initTagData()

	staticDir := filepath.Join(filepath.Dir(loginFlag), "..", "static")
	// Resolve to the directory next to the binary.
	exe, _ := os.Executable()
	staticDir = filepath.Join(filepath.Dir(exe), "static")
	if _, err := os.Stat(staticDir); err != nil {
		// Fallback for `go run .`
		staticDir = "static"
	}

	// Pre-load disk cache so the UI has data immediately.
	if cached, _ := loadEmailCache(); len(cached) > 0 {
		result := rebuildFromCached(cached)
		mu.Lock()
		emailCache = &result
		mu.Unlock()
		slog.Info("startup cache restored", "emails", len(cached))
	}

	const port = "8000"
	slog.Info("GmailCleaner ready", "url", "http://localhost:"+port)
	if err := http.ListenAndServe(":"+port, newMux(staticDir)); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
