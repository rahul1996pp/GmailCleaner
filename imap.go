package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// ── Paths ─────────────────────────────────────────────────────────────────────

const (
	imapHost = "imap.gmail.com"
	imapPort = "993"

	folderAll   = "[Gmail]/All Mail"
	folderSpam  = "[Gmail]/Spam"
	folderTrash = "[Gmail]/Trash"
)

var (
	baseDir   string // root browser_data/ — shared across all profiles
	dataDir   string // per-profile directory (profiles/<email>/)
	loginFlag string
	credsFile string
	cacheFile string
)

func initPaths() {
	exe, err := os.Executable()
	base := "."
	if err == nil {
		base = filepath.Dir(exe)
	}
	baseDir = filepath.Join(base, "browser_data")
	dataDir = baseDir // default until a profile is activated
	loginFlag = filepath.Join(baseDir, ".logged_in")
	credsFile = filepath.Join(baseDir, "imap_creds.json")
	cacheFile = filepath.Join(baseDir, "email_cache.json")
}

// switchProfile sets dataDir to a per-user profile directory and updates all
// derived paths.  Call this whenever the active account changes.
func switchProfile(email string) {
	if email == "" {
		dataDir = baseDir
	} else {
		// Sanitise email for use as a directory name.
		safe := strings.ReplaceAll(email, "@", "_at_")
		safe = strings.ReplaceAll(safe, "/", "_")
		safe = strings.ReplaceAll(safe, "\\", "_")
		dataDir = filepath.Join(baseDir, "profiles", safe)
	}
	_ = os.MkdirAll(dataDir, 0700)
	loginFlag = filepath.Join(dataDir, ".logged_in")
	credsFile = filepath.Join(dataDir, "imap_creds.json")
	cacheFile = filepath.Join(dataDir, "email_cache.json")
}

// ── Data types ────────────────────────────────────────────────────────────────

type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type Email struct {
	ID          string `json:"id"`
	SenderName  string `json:"sender_name"`
	SenderEmail string `json:"sender_email"`
	Domain      string `json:"domain"`
	Subject     string `json:"subject"`
	Snippet     string `json:"snippet"`
	Date        string `json:"date"`
	Unread      bool   `json:"unread"`
	HasAttach   bool   `json:"has_attachment"`
	GmailLabel  string `json:"gmail_label"`
	Category    string `json:"category,omitempty"`
}

type diskCache struct {
	Emails  []Email             `json:"emails"`
	UIDSets map[string][]string `json:"uid_sets"`
}

// EmailBody is the JSON response for /api/email_body.
type EmailBody struct {
	Subject  string `json:"subject"`
	From     string `json:"from"`
	To       string `json:"to"`
	Date     string `json:"date"`
	Body     string `json:"body"`
	BodyHTML string `json:"body_html,omitempty"`
	Error    string `json:"error,omitempty"`
}

// DeleteResult is the JSON response for /api/delete.
type DeleteResult struct {
	Deleted int      `json:"deleted"`
	Errors  []string `json:"errors"`
}

// ── Credentials ───────────────────────────────────────────────────────────────

func saveCreds(email, password string) error {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	data, _ := json.Marshal(Credentials{Email: email, Password: password})
	return os.WriteFile(credsFile, data, 0600)
}

func loadCreds() (*Credentials, error) {
	data, err := os.ReadFile(credsFile)
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ── IMAP connection ───────────────────────────────────────────────────────────

// imapConn wraps a TLS connection with correct IMAP literal handling.
// IMAP can embed raw binary blobs mid-response as {N}\r\n<N bytes>, which
// must be read with exact byte counts rather than line-by-line reads.
type imapConn struct {
	conn   net.Conn
	tail   []byte // buffered bytes not yet consumed
	buf    []byte // scratch read buffer
	tagSeq int
}

func dialIMAP() (*imapConn, error) {
	tc, err := tls.Dial("tcp", imapHost+":"+imapPort, nil)
	if err != nil {
		return nil, fmt.Errorf("dial imap.gmail.com:993: %w", err)
	}
	c := &imapConn{conn: tc, buf: make([]byte, 32*1024)}
	// Consume server greeting line.
	if _, err := c.readLine(); err != nil {
		tc.Close()
		return nil, fmt.Errorf("imap greeting: %w", err)
	}
	return c, nil
}

func (c *imapConn) Close() { _ = c.conn.Close() }

func (c *imapConn) nextTag() string {
	c.tagSeq++
	return fmt.Sprintf("T%04d", c.tagSeq)
}

// refill appends more network bytes to c.tail.
func (c *imapConn) refill() error {
	n, err := c.conn.Read(c.buf)
	if n > 0 {
		c.tail = append(c.tail, c.buf[:n]...)
	}
	return err
}

// readLine returns the next CRLF-terminated line (without the CRLF).
func (c *imapConn) readLine() (string, error) {
	for {
		for i := 0; i < len(c.tail)-1; i++ {
			if c.tail[i] == '\r' && c.tail[i+1] == '\n' {
				line := string(c.tail[:i])
				c.tail = c.tail[i+2:]
				return line, nil
			}
		}
		if err := c.refill(); err != nil {
			return "", err
		}
	}
}

// readExact reads exactly n bytes, possibly spanning multiple network reads.
func (c *imapConn) readExact(n int) ([]byte, error) {
	for len(c.tail) < n {
		if err := c.refill(); err != nil && len(c.tail) < n {
			return c.tail, err
		}
	}
	out := make([]byte, n)
	copy(out, c.tail[:n])
	c.tail = c.tail[n:]
	return out, nil
}

// literalMarkRe matches {N} at the end of an IMAP response line.
var literalMarkRe = regexp.MustCompile(`\{(\d+)\}\s*$`)
var uidRe = regexp.MustCompile(`\bUID (\d+)`)
var flagsRe = regexp.MustCompile(`FLAGS \(([^)]*)\)`)

// readUntilTagged reads IMAP response lines, correctly consuming literal blobs,
// until the line starting with `tag ` is found.
// Literal blobs are appended to the returned slice as individual string elements.
func (c *imapConn) readUntilTagged(tag string) ([]string, string, error) {
	var lines []string
	for {
		line, err := c.readLine()
		if err != nil {
			return lines, "", fmt.Errorf("imap readLine: %w", err)
		}
		if strings.HasPrefix(line, tag+" ") {
			return lines, line, nil
		}
		lines = append(lines, line)

		// Literal: {N}\r\n → read exactly N raw bytes next.
		if m := literalMarkRe.FindStringSubmatch(line); m != nil {
			size, _ := strconv.Atoi(m[1])
			if size > 0 {
				blob, err := c.readExact(size)
				if err != nil {
					return lines, "", fmt.Errorf("imap literal read: %w", err)
				}
				lines = append(lines, string(blob))
			}
		}
	}
}

// cmd sends one IMAP command and returns (untagged lines, tagged line, error).
func (c *imapConn) cmd(format string, args ...any) ([]string, string, error) {
	tag := c.nextTag()
	payload := fmt.Sprintf(format, args...)
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, payload); err != nil {
		return nil, "", err
	}
	return c.readUntilTagged(tag)
}

// isOK returns true when the tagged response line indicates success.
func isOK(tagged string) bool {
	f := strings.SplitN(tagged, " ", 3)
	return len(f) >= 2 && strings.EqualFold(f[1], "OK")
}

// quoteArg returns an IMAP-quoted string.
func quoteArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// ── IMAP command wrappers ────────────────────────────────────────────────────

func imapLogin(c *imapConn, email, password string) error {
	_, tagged, err := c.cmd("LOGIN %s %s", quoteArg(email), quoteArg(password))
	if err != nil {
		return err
	}
	if !isOK(tagged) {
		return fmt.Errorf("LOGIN rejected: %s", tagged)
	}
	return nil
}

func imapExamine(c *imapConn, folder string) error {
	_, tagged, err := c.cmd("EXAMINE %s", quoteArg(folder))
	if err != nil {
		return err
	}
	if !isOK(tagged) {
		return fmt.Errorf("EXAMINE %q: %s", folder, tagged)
	}
	return nil
}

func imapSelect(c *imapConn, folder string) error {
	_, tagged, err := c.cmd("SELECT %s", quoteArg(folder))
	if err != nil {
		return err
	}
	if !isOK(tagged) {
		return fmt.Errorf("SELECT %q: %s", folder, tagged)
	}
	return nil
}

func imapUIDSearch(c *imapConn) ([]string, error) {
	lines, tagged, err := c.cmd("UID SEARCH ALL")
	if err != nil {
		return nil, err
	}
	if !isOK(tagged) {
		return nil, fmt.Errorf("UID SEARCH: %s", tagged)
	}
	var uids []string
	for _, l := range lines {
		if strings.HasPrefix(l, "* SEARCH") {
			uids = append(uids, strings.Fields(l)[2:]...)
		}
	}
	return uids, nil
}

func imapLogout(c *imapConn) {
	_, _, _ = c.cmd("LOGOUT")
}

// ── Header decoding ───────────────────────────────────────────────────────────

var mimeDecoder = new(mime.WordDecoder)

func decodeHeader(v string) string {
	if v == "" {
		return ""
	}
	out, err := mimeDecoder.DecodeHeader(v)
	if err != nil {
		return v
	}
	return out
}

// extractHeader extracts a named header value from a raw RFC 2822 header block
// without compiling a regexp on every call. Handles folded headers.
// Case-insensitive on the header name.
func extractHeader(block, name string) string {
	needle := strings.ToLower(name) + ":"
	lower := strings.ToLower(block)
	start := 0
	for {
		idx := strings.Index(lower[start:], needle)
		if idx < 0 {
			return ""
		}
		abs := start + idx
		// Must be at start of block or immediately after \r\n
		if abs != 0 && (abs < 2 || block[abs-2] != '\r' || block[abs-1] != '\n') {
			start = abs + len(needle)
			continue
		}
		// Advance past "name:"
		pos := abs + len(needle)
		// Skip optional leading whitespace
		for pos < len(block) && (block[pos] == ' ' || block[pos] == '\t') {
			pos++
		}
		// Collect value, unfolding continuation lines
		var sb strings.Builder
		for pos < len(block) {
			end := strings.Index(block[pos:], "\r\n")
			if end < 0 {
				sb.WriteString(block[pos:])
				break
			}
			sb.WriteString(block[pos : pos+end])
			pos += end + 2
			// Folded continuation line starts with space/tab
			if pos < len(block) && (block[pos] == ' ' || block[pos] == '\t') {
				sb.WriteByte(' ')
				for pos < len(block) && (block[pos] == ' ' || block[pos] == '\t') {
					pos++
				}
				continue
			}
			break
		}
		return decodeHeader(strings.TrimSpace(sb.String()))
	}
}

var fromAngleRe = regexp.MustCompile(`^(.*?)\s*<([^>]+)>\s*$`)

func parseFrom(from string) (name, addr string) {
	from = strings.TrimSpace(from)
	if m := fromAngleRe.FindStringSubmatch(from); m != nil {
		return strings.Trim(m[1], `"' `), strings.TrimSpace(m[2])
	}
	if a, err := mail.ParseAddress(from); err == nil {
		return a.Name, a.Address
	}
	return "", from
}

var domainRe = regexp.MustCompile(`@([\w.-]+)`)

func extractDomain(addr string) string {
	m := domainRe.FindStringSubmatch(addr)
	if m == nil {
		return "unknown"
	}
	parts := strings.Split(strings.ToLower(m[1]), ".")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return m[1]
}

var (
	encWordRe = regexp.MustCompile(`=\?[^?]+\?[BbQq]\?[^?]+\?=`)
	whitespRe = regexp.MustCompile(`\s+`)
)

func cleanSnippet(raw string) string {
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "")
	}
	raw = encWordRe.ReplaceAllString(raw, "")
	raw = whitespRe.ReplaceAllString(raw, " ")
	raw = strings.TrimSpace(raw)
	runes := []rune(raw)
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return string(runes)
}

// ── FETCH response parser ─────────────────────────────────────────────────────

// After readUntilTagged, a UID FETCH response looks like:
//
//   lines[0] = "* 1 FETCH (UID 42 FLAGS (\\Seen) BODY[HEADER.FIELDS …] {N}"
//   lines[1] = "<N bytes of raw header>"
//   lines[2] = " BODY[TEXT]<0> {M}"
//   lines[3] = "<M bytes of text snippet>"
//   lines[4] = ")"
//   lines[5] = "* 2 FETCH (UID 43 …"
//   …
//
// We group lines into per-message blocks and extract fields from each block.

var fetchStartRe = regexp.MustCompile(`^\* \d+ FETCH \(`)

func parseFetchLines(lines []string, folderKey, label string, seenIDs map[string]bool) []Email {
	// Split into per-message blocks.
	var starts []int
	for i, l := range lines {
		if fetchStartRe.MatchString(l) {
			starts = append(starts, i)
		}
	}

	var results []Email
	for i, start := range starts {
		end := len(lines)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		block := lines[start:end]
		e := parseSingleFetch(block, folderKey, label)
		if e == nil || seenIDs[e.ID] {
			continue
		}
		seenIDs[e.ID] = true
		results = append(results, *e)
	}
	return results
}

func parseSingleFetch(lines []string, folderKey, label string) *Email {
	if len(lines) == 0 {
		return nil
	}
	leader := lines[0]

	// Extract UID from the leader line e.g. "* 3 FETCH (UID 42 FLAGS (…) …)"
	uidM := uidRe.FindStringSubmatch(leader)
	if uidM == nil {
		return nil
	}
	uid := uidM[1]

	flagsM := flagsRe.FindStringSubmatch(leader)
	unread := true
	if flagsM != nil {
		unread = !strings.Contains(flagsM[1], `\Seen`)
	}

	// Walk the block lines: a line ending in {N} means the next element is
	// the literal blob of N bytes (already read by readUntilTagged).
	headerText := ""
	snippetText := ""
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if literalMarkRe.MatchString(l) && i+1 < len(lines) {
			blob := lines[i+1]
			i++ // consume blob line
			if strings.Contains(l, "BODY[HEADER") {
				headerText = blob
			} else if strings.Contains(l, "BODY[TEXT]") {
				snippetText = blob
			}
		}
	}

	subj := extractHeader(headerText, "Subject")
	from := extractHeader(headerText, "From")
	date := extractHeader(headerText, "Date")
	senderName, senderEmail := parseFrom(from)

	// Detect attachments from BODYSTRUCTURE in the leader line + subsequent lines.
	hasAttach := false
	bsText := strings.Join(lines, " ")
	bsLower := strings.ToLower(bsText)
	if strings.Contains(bsLower, "\"attachment\"") || strings.Contains(bsLower, "attachment") {
		// Look for Content-Disposition: attachment in BODYSTRUCTURE
		if strings.Contains(bsLower, "\"attachment\"") {
			hasAttach = true
		} else {
			// Also detect common attachment MIME types that aren't inline
			attachRe := regexp.MustCompile(`(?i)"(application|image|audio|video)/[^"]+"\s+NIL\s+NIL\s+"[^"]*"\s+\d+`)
			hasAttach = attachRe.MatchString(bsText)
		}
	}

	return &Email{
		ID:          fmt.Sprintf("imap:%s:%s", folderKey, uid),
		SenderName:  senderName,
		SenderEmail: senderEmail,
		Domain:      extractDomain(senderEmail),
		Subject:     subj,
		Snippet:     cleanSnippet(snippetText),
		Date:        date,
		Unread:      unread,
		HasAttach:   hasAttach,
		GmailLabel:  label,
	}
}

// ── Batch fetch ───────────────────────────────────────────────────────────────

// fetchHeadersBatch fetches headers for uids using up to maxConns parallel
// IMAP connections, each handling a non-overlapping slice of batches.
// This turns 200 sequential round-trips (for 40k emails) into ~5 parallel
// streams, reducing wall-clock time by ~10x.
func fetchHeadersBatch(
	uids []string,
	folderKey, label string,
	seenIDsMu *sync.Mutex,
	seenIDs map[string]bool,
) ([]Email, error) {
	const (
		batchSize = 500 // larger batches = fewer round-trips
		maxConns  = 5   // Gmail allows ~10 concurrent IMAP connections
	)

	if len(uids) == 0 {
		return nil, nil
	}

	// Divide uid list into maxConns equal slices.
	type work struct{ start, end int }
	var jobs []work
	sliceSize := (len(uids) + maxConns - 1) / maxConns
	for i := 0; i < len(uids); i += sliceSize {
		end := i + sliceSize
		if end > len(uids) {
			end = len(uids)
		}
		jobs = append(jobs, work{i, end})
	}

	type result struct {
		emails []Email
		err    error
	}
	resultsCh := make(chan result, len(jobs))
	done := int64(0)
	total := int64(len(uids))

	for _, job := range jobs {
		job := job
		go func() {
			conn, err := connect()
			if err != nil {
				resultsCh <- result{err: err}
				return
			}
			defer func() { imapLogout(conn); conn.Close() }()

			if err := imapExamine(conn, keyToFolder[folderKey]); err != nil {
				resultsCh <- result{err: err}
				return
			}

			var localEmails []Email
			slice := uids[job.start:job.end]
			for i := 0; i < len(slice); i += batchSize {
				end := i + batchSize
				if end > len(slice) {
					end = len(slice)
				}
				uidStr := strings.Join(slice[i:end], ",")

				lines, tagged, err := conn.cmd(
					"UID FETCH %s (FLAGS BODYSTRUCTURE BODY.PEEK[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)])",
					uidStr,
				)
				if err != nil || !isOK(tagged) {
					slog.Error("batch fetch error", "folder", label, "err", err, "resp", tagged)
					continue
				}

				// Parse without holding the lock, then merge atomically.
				var tmpSeen = map[string]bool{}
				batch := parseFetchLines(lines, folderKey, label, tmpSeen)

				seenIDsMu.Lock()
				var fresh []Email
				for _, e := range batch {
					if !seenIDs[e.ID] {
						seenIDs[e.ID] = true
						fresh = append(fresh, e)
					}
				}
				seenIDsMu.Unlock()
				localEmails = append(localEmails, fresh...)

				n := atomic.AddInt64(&done, int64(end-i))
				slog.Info("imap progress", "folder", label,
					"done", n, "total", total, "parsed", len(localEmails))
			}
			resultsCh <- result{emails: localEmails}
		}()
	}

	var allEmails []Email
	var firstErr error
	for range jobs {
		r := <-resultsCh
		if r.err != nil && firstErr == nil {
			firstErr = r.err
		}
		allEmails = append(allEmails, r.emails...)
	}
	return allEmails, firstErr
}

// ── Disk cache ────────────────────────────────────────────────────────────────

func loadEmailCache() ([]Email, map[string][]string) {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, map[string][]string{}
	}
	var dc diskCache
	if err := json.Unmarshal(data, &dc); err != nil {
		slog.Warn("cache corrupt, starting fresh", "err", err)
		return nil, map[string][]string{}
	}
	if dc.UIDSets == nil {
		dc.UIDSets = map[string][]string{}
	}
	slog.Info("cache loaded", "emails", len(dc.Emails))
	return dc.Emails, dc.UIDSets
}

func saveEmailCache(emails []Email, uidSets map[string][]string) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		slog.Warn("cannot create dataDir", "err", err)
		return
	}
	data, err := json.Marshal(diskCache{Emails: emails, UIDSets: uidSets})
	if err != nil {
		slog.Warn("cache marshal error", "err", err)
		return
	}
	// Atomic write.
	tmp := cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		slog.Warn("cache write error", "err", err)
		return
	}
	if err := os.Rename(tmp, cacheFile); err != nil {
		slog.Warn("cache rename error", "err", err)
	}
	slog.Info("cache saved", "emails", len(emails))
}

// ── Login / Logout ────────────────────────────────────────────────────────────

func loginSync(email, password string) (status, message string) {
	if email == "" || password == "" {
		return "error", "Email and App Password are required."
	}
	c, err := dialIMAP()
	if err != nil {
		return "error", "Connection error: " + err.Error()
	}
	defer c.Close()

	if err := imapLogin(c, email, password); err != nil {
		return "error", "Authentication failed: " + err.Error()
	}
	imapLogout(c)

	if err := saveCreds(email, password); err != nil {
		return "error", "Could not save credentials: " + err.Error()
	}
	_ = os.MkdirAll(dataDir, 0700)
	_ = os.WriteFile(loginFlag, []byte(email), 0600)
	slog.Info("login ok", "email", email)
	return "logged_in", "Connected to Gmail via IMAP!"
}

// verifyIMAPCreds checks IMAP credentials without saving anything to disk.
func verifyIMAPCreds(email, password string) error {
	c, err := dialIMAP()
	if err != nil {
		return fmt.Errorf("connection error: %w", err)
	}
	defer c.Close()
	if err := imapLogin(c, email, password); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	imapLogout(c)
	return nil
}

func logoutSync() {
	for _, p := range []string{loginFlag, credsFile} {
		_ = os.Remove(p)
	}
}

func isLoggedIn() bool {
	_, err := os.Stat(loginFlag)
	return err == nil
}

// ── Folder definitions ────────────────────────────────────────────────────────

type folderDef struct {
	imap      string
	label     string
	folderKey string
}

var imapFolders = []folderDef{
	{folderAll, "All Mail", "all"},
	{folderSpam, "Spam", "spam"},
}

var keyToFolder = map[string]string{
	"all":  folderAll,
	"spam": folderSpam,
}

func connect() (*imapConn, error) {
	creds, err := loadCreds()
	if err != nil {
		return nil, fmt.Errorf("no saved credentials — please log in first")
	}
	c, err := dialIMAP()
	if err != nil {
		return nil, err
	}
	if err := imapLogin(c, creds.Email, creds.Password); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// ── Fetch all emails (incremental) ───────────────────────────────────────────

func fetchEmailsSync(fullRefresh bool) ([]Email, error) {
	slog.Info("fetch start", "full_refresh", fullRefresh)

	cachedEmails, cachedUIDSets := loadEmailCache()
	if fullRefresh {
		cachedEmails = nil
		cachedUIDSets = map[string][]string{}
	}

	cachedIDs := make(map[string]bool, len(cachedEmails))
	for _, e := range cachedEmails {
		cachedIDs[e.ID] = true
	}

	c, err := connect()
	if err != nil {
		return nil, err
	}
	defer func() { imapLogout(c); c.Close() }()

	finalUIDSets := make(map[string][]string)
	var newEmails []Email

	for _, fd := range imapFolders {
		knownSet := sliceToSet(cachedUIDSets[fd.folderKey])

		if err := imapExamine(c, fd.imap); err != nil {
			slog.Warn("EXAMINE failed, skipping", "folder", fd.label, "err", err)
			finalUIDSets[fd.folderKey] = setToSlice(knownSet)
			continue
		}

		serverUIDs, err := imapUIDSearch(c)
		if err != nil {
			slog.Warn("UID SEARCH failed", "folder", fd.label, "err", err)
			finalUIDSets[fd.folderKey] = setToSlice(knownSet)
			continue
		}

		serverSet := sliceToSet(serverUIDs)
		finalUIDSets[fd.folderKey] = serverUIDs

		newSet := setDiff(serverSet, knownSet)
		slog.Info("uid sync", "folder", fd.label,
			"server", len(serverUIDs), "cached", len(knownSet),
			"new", len(newSet), "removed", len(setDiff(knownSet, serverSet)))

		if len(newSet) == 0 {
			continue
		}

		newUIDs := setToSlice(newSet)
		sort.Slice(newUIDs, func(i, j int) bool {
			a, _ := strconv.Atoi(newUIDs[i])
			b, _ := strconv.Atoi(newUIDs[j])
			return a < b
		})

		var seenMu sync.Mutex
		found, _ := fetchHeadersBatch(newUIDs, fd.folderKey, fd.label, &seenMu, cachedIDs)
		newEmails = append(newEmails, found...)
	}

	// Build valid ID set from current server state, then merge.
	validIDs := make(map[string]bool)
	for fkey, uids := range finalUIDSets {
		for _, uid := range uids {
			validIDs[fmt.Sprintf("imap:%s:%s", fkey, uid)] = true
		}
	}
	var kept []Email
	for _, e := range cachedEmails {
		if validIDs[e.ID] {
			kept = append(kept, e)
		}
	}
	merged := append(kept, newEmails...)
	saveEmailCache(merged, finalUIDSets)
	slog.Info("fetch done", "total", len(merged), "new", len(newEmails))
	return merged, nil
}

// ── Email body ────────────────────────────────────────────────────────────────

var (
	styleScriptRe = regexp.MustCompile(`(?is)<(style|script)[^>]*>.*?</(style|script)>`)
	blockTagRe    = regexp.MustCompile(`(?i)<(?:br|p|div|tr|li|h[1-6]|hr)[^>]*/?>`)
	allTagRe      = regexp.MustCompile(`<[^>]+>`)
	multiNLRe     = regexp.MustCompile(`\n{3,}`)
	multiSpcRe    = regexp.MustCompile(` {3,}`)
)

func stripHTML(h string) string {
	s := styleScriptRe.ReplaceAllString(h, "")
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = allTagRe.ReplaceAllString(s, "")
	for _, p := range [][2]string{
		{"&nbsp;", " "}, {"&amp;", "&"}, {"&lt;", "<"},
		{"&gt;", ">"}, {"&quot;", `"`}, {"&#39;", "'"}, {"&apos;", "'"},
	} {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	s = multiSpcRe.ReplaceAllString(s, "  ")
	s = multiNLRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// decodeTransferEncoding decodes base64 or quoted-printable content.
func decodeTransferEncoding(data []byte, encoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(
			strings.Join(strings.Fields(string(data)), ""),
		)
		if err == nil {
			return decoded
		}
		return data
	case "quoted-printable":
		rd := quotedprintable.NewReader(bytes.NewReader(data))
		out, err := io.ReadAll(rd)
		if err == nil {
			return out
		}
		return data
	default:
		return data
	}
}

// extractMIMEParts walks a MIME multipart message and returns text/plain and text/html bodies.
func extractMIMEParts(msg *mail.Message) (plain, html string) {
	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		// Fallback: read body as-is.
		raw, _ := io.ReadAll(msg.Body)
		enc := msg.Header.Get("Content-Transfer-Encoding")
		decoded := decodeTransferEncoding(raw, enc)
		return string(decoded), ""
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			raw, _ := io.ReadAll(msg.Body)
			return string(raw), ""
		}
		mr := multipart.NewReader(msg.Body, boundary)
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			pCT := p.Header.Get("Content-Type")
			pMedia, pParams, _ := mime.ParseMediaType(pCT)
			pEnc := p.Header.Get("Content-Transfer-Encoding")
			raw, _ := io.ReadAll(p)
			decoded := decodeTransferEncoding(raw, pEnc)

			if strings.HasPrefix(pMedia, "multipart/") {
				// Nested multipart (e.g. multipart/alternative inside multipart/mixed)
				nestedBoundary := pParams["boundary"]
				if nestedBoundary != "" {
					nr := multipart.NewReader(bytes.NewReader(decoded), nestedBoundary)
					for {
						np, nerr := nr.NextPart()
						if nerr != nil {
							break
						}
						npCT := np.Header.Get("Content-Type")
						npMedia, _, _ := mime.ParseMediaType(npCT)
						npEnc := np.Header.Get("Content-Transfer-Encoding")
						npRaw, _ := io.ReadAll(np)
						npDecoded := decodeTransferEncoding(npRaw, npEnc)
						switch {
						case strings.HasPrefix(npMedia, "text/plain") && plain == "":
							plain = string(npDecoded)
						case strings.HasPrefix(npMedia, "text/html") && html == "":
							html = string(npDecoded)
						}
					}
				}
				continue
			}

			switch {
			case strings.HasPrefix(pMedia, "text/plain") && plain == "":
				plain = string(decoded)
			case strings.HasPrefix(pMedia, "text/html") && html == "":
				html = string(decoded)
			}
		}
		return plain, html
	}

	// Not multipart — single body.
	raw, _ := io.ReadAll(msg.Body)
	enc := msg.Header.Get("Content-Transfer-Encoding")
	decoded := decodeTransferEncoding(raw, enc)
	if strings.HasPrefix(mediaType, "text/html") {
		return "", string(decoded)
	}
	return string(decoded), ""
}

func fetchEmailBodySync(emailID string) EmailBody {
	parts := strings.SplitN(emailID, ":", 3)
	if len(parts) != 3 || parts[0] != "imap" {
		return EmailBody{Error: "invalid email ID format"}
	}
	folder, exists := keyToFolder[parts[1]]
	if !exists {
		return EmailBody{Error: "unknown folder key: " + parts[1]}
	}
	uid := parts[2]

	c, err := connect()
	if err != nil {
		return EmailBody{Error: err.Error()}
	}
	defer func() { imapLogout(c); c.Close() }()

	if err := imapExamine(c, folder); err != nil {
		return EmailBody{Error: "cannot open folder: " + err.Error()}
	}

	lines, tagged, err := c.cmd("UID FETCH %s (BODY.PEEK[])", uid)
	if err != nil || !isOK(tagged) {
		msg := "FETCH failed"
		if err != nil {
			msg += ": " + err.Error()
		}
		return EmailBody{Error: msg}
	}

	// Find the literal blob that follows the "BODY[] {N}" line.
	rawMsg := ""
	for i, l := range lines {
		if strings.Contains(l, "BODY[]") && literalMarkRe.MatchString(l) && i+1 < len(lines) {
			rawMsg = lines[i+1]
			break
		}
	}
	if rawMsg == "" {
		return EmailBody{Error: "no body in IMAP response"}
	}

	msg, err := mail.ReadMessage(strings.NewReader(rawMsg))
	if err != nil {
		body := rawMsg
		if len([]rune(body)) > 15000 {
			body = string([]rune(body)[:15000])
		}
		return EmailBody{Subject: "(parse error)", Body: body}
	}

	plainBody, htmlBody := extractMIMEParts(msg)

	// Provide plain text fallback if we only got HTML.
	plainText := plainBody
	if plainText == "" && htmlBody != "" {
		plainText = stripHTML(htmlBody)
	}

	const maxRunes = 200000 // generous limit for HTML
	if len([]rune(htmlBody)) > maxRunes {
		htmlBody = string([]rune(htmlBody)[:maxRunes])
	}
	if len([]rune(plainText)) > 15000 {
		plainText = string([]rune(plainText)[:15000])
	}

	return EmailBody{
		Subject:  decodeHeader(msg.Header.Get("Subject")),
		From:     decodeHeader(msg.Header.Get("From")),
		To:       decodeHeader(msg.Header.Get("To")),
		Date:     msg.Header.Get("Date"),
		Body:     plainText,
		BodyHTML: htmlBody,
	}
}

// ── Delete emails ─────────────────────────────────────────────────────────────

func deleteEmailsSync(emailIDs []string) DeleteResult {
	byFolder := make(map[string][]string)
	var errs []string

	for _, eid := range emailIDs {
		parts := strings.SplitN(eid, ":", 3)
		if len(parts) != 3 || parts[0] != "imap" {
			errs = append(errs, "invalid id: "+eid)
			continue
		}
		folder, exists := keyToFolder[parts[1]]
		if !exists {
			errs = append(errs, "unknown folder key in: "+eid)
			continue
		}
		byFolder[folder] = append(byFolder[folder], parts[2])
	}

	if len(byFolder) == 0 {
		return DeleteResult{Errors: errs}
	}

	c, err := connect()
	if err != nil {
		return DeleteResult{Errors: append(errs, err.Error())}
	}
	defer func() { imapLogout(c); c.Close() }()

	deleted := 0
	for folder, uids := range byFolder {
		if err := imapSelect(c, folder); err != nil {
			errs = append(errs, "select "+folder+": "+err.Error())
			continue
		}
		uidStr := strings.Join(uids, ",")
		if _, t, err := c.cmd("UID COPY %s %s", uidStr, quoteArg(folderTrash)); err != nil || !isOK(t) {
			slog.Warn("UID COPY to Trash failed", "folder", folder)
		}
		if _, t, err := c.cmd(`UID STORE %s +FLAGS.SILENT \Deleted`, uidStr); err != nil || !isOK(t) {
			slog.Warn("UID STORE \\Deleted failed", "folder", folder)
		}
		if _, t, err := c.cmd("EXPUNGE"); err != nil || !isOK(t) {
			slog.Warn("EXPUNGE failed", "folder", folder)
		}
		deleted += len(uids)
		slog.Info("moved to trash", "folder", folder, "count", len(uids))
	}

	return DeleteResult{Deleted: deleted, Errors: errs}
}

// emptyTrashSync selects [Gmail]/Trash, flags all messages as \Deleted, and
// expunges them permanently.
func emptyTrashSync() (int, error) {
	c, err := connect()
	if err != nil {
		return 0, err
	}
	defer func() { imapLogout(c); c.Close() }()

	if err := imapSelect(c, folderTrash); err != nil {
		return 0, fmt.Errorf("select trash: %w", err)
	}

	// Flag all messages in the trash as \Deleted
	if _, t, err := c.cmd(`STORE 1:* +FLAGS.SILENT \Deleted`); err != nil || !isOK(t) {
		return 0, fmt.Errorf("STORE \\Deleted on trash failed")
	}
	if _, t, err := c.cmd("EXPUNGE"); err != nil || !isOK(t) {
		return 0, fmt.Errorf("EXPUNGE trash failed")
	}

	slog.Info("emptied trash")
	return 0, nil // Gmail doesn't tell us how many were expunged
}

// ── Set utilities ─────────────────────────────────────────────────────────────

func sliceToSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

func setToSlice(m map[string]bool) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	return s
}

func setDiff(a, b map[string]bool) map[string]bool {
	d := make(map[string]bool)
	for k := range a {
		if !b[k] {
			d[k] = true
		}
	}
	return d
}

func intMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}
