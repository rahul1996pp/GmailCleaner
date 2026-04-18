package main

import (
	"encoding/json"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// ── Keyword sets ──────────────────────────────────────────────────────────────

var newsletterKeywords = []string{
	"newsletter", "weekly digest", "daily digest", "roundup", "recap",
	"bulletin", "update", "issue #", "vol.", "edition", "dispatch",
	"briefing", "morning brief", "weekly brief",
}

var promotionalKeywords = []string{
	"sale", "off", "discount", "deal", "offer", "promo", "coupon",
	"limited time", "expires", "flash sale", "clearance", "% off",
	"save ", "free shipping", "buy now", "shop now", "exclusive offer",
	"members only", "special price", "today only", "last chance",
	"black friday", "cyber monday", "seasonal offer",
}

var socialDomains = map[string]bool{
	"facebook.com": true, "facebookmail.com": true, "twitter.com": true,
	"linkedin.com": true, "instagram.com": true, "pinterest.com": true,
	"tiktok.com": true, "reddit.com": true, "youtube.com": true,
	"snapchat.com": true, "whatsapp.com": true, "telegram.org": true,
	"quora.com": true, "medium.com": true,
}

var socialKeywords = []string{
	"connected with you", "mentioned you", "liked your", "commented on",
	"new follower", "friend request", "tagged you", "replied to",
	"sent you a message", "invited you",
}

var notificationKeywords = []string{
	"alert", "notification", "reminder", "your account", "action required",
	"verify", "confirm", "security", "password", "login", "otp",
	"billing", "invoice", "receipt", "order", "shipment", "tracking",
	"delivery", "subscription", "renewal",
}

var campaignSenders = []string{
	"no-reply", "noreply", "do-not-reply", "donotreply", "campaigns",
	"marketing", "mailer", "newsletter", "news", "info", "hello",
	"hi", "team", "support", "notifications", "alerts",
	"automated", "bounce", "postmaster",
}

var spamKeywords = []string{
	"you have won", "congratulations", "claim your prize", "lottery",
	"winner", "free gift", "inheritance", "million dollar",
	"wire transfer", "nigerian", "bank transfer", "click here to claim",
	"make money fast", "work from home", "earn $", "earn money",
	"100% free", "guarantee", "risk free", "act now", "urgent",
	"limited spots", "selected winner",
}

var transactionalKeywords = []string{
	"order confirmation", "receipt", "invoice", "payment", "subscription",
	"your booking", "reservation", "ticket", "itinerary", "account statement",
	"transaction", "deposit", "withdrawal", "refund",
}

var knownSpamDomains = map[string]bool{
	"tempmail.com": true, "mailinator.com": true, "guerrillamail.com": true,
	"throwam.com": true, "yopmail.com": true, "sharklasers.com": true,
	"guerrillamailblock.com": true,
}

var validCategories = map[string]bool{
	"Newsletter": true, "Promotional": true, "Social": true,
	"Notification": true, "Transactional": true, "Campaign": true,
	"Spam": true, "General": true,
}

const (
	CatNewsletter    = "Newsletter"
	CatPromotional   = "Promotional"
	CatSocial        = "Social"
	CatNotification  = "Notification"
	CatTransactional = "Transactional"
	CatCampaign      = "Campaign"
	CatSpam          = "Spam"
	CatGeneral       = "General"
)

// ── Domain cache ──────────────────────────────────────────────────────────────

func domainCacheFile() string {
	return dataDir + "/domain_cats.json"
}

func loadDomainCache() map[string]string {
	data, err := os.ReadFile(domainCacheFile())
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]string{}
	}
	return m
}

func saveDomainCache(cache map[string]string) {
	_ = os.MkdirAll(dataDir, 0755)
	data, _ := json.MarshalIndent(cache, "", "  ")
	_ = os.WriteFile(domainCacheFile(), data, 0644)
}

// ── Core categorizer ──────────────────────────────────────────────────────────

func matchesAny(text string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func categorizeOne(e *Email) string {
	subject := strings.ToLower(e.Subject)
	snippet := strings.ToLower(e.Snippet)
	senderEmail := strings.ToLower(e.SenderEmail)
	domain := strings.ToLower(e.Domain)
	gmailLabel := strings.ToLower(e.GmailLabel)

	combined := subject + " " + snippet
	senderLocal := senderEmail
	if at := strings.Index(senderEmail, "@"); at >= 0 {
		senderLocal = senderEmail[:at]
	}

	switch {
	case gmailLabel == "spam":
		return CatSpam
	case knownSpamDomains[domain]:
		return CatSpam
	case matchesAny(combined, spamKeywords):
		return CatSpam
	case matchesAny(combined, promotionalKeywords):
		return CatPromotional
	case matchesAny(combined, newsletterKeywords):
		return CatNewsletter
	case matchesAny(senderLocal, []string{"newsletter", "digest", "news"}):
		return CatNewsletter
	case socialDomains[domain]:
		return CatSocial
	case matchesAny(combined, socialKeywords):
		return CatSocial
	case matchesAny(combined, transactionalKeywords):
		return CatTransactional
	case matchesAny(combined, notificationKeywords):
		return CatNotification
	case matchesAny(senderLocal, campaignSenders):
		return CatCampaign
	}
	return CatGeneral
}

// ── Structured result ─────────────────────────────────────────────────────────

// CategorizeResult is the full in-memory email store served to the UI.
// ByCategory and ByDomain store integer indices into Emails to avoid
// copying large Email structs multiple times.
type CategorizeResult struct {
	Emails     []Email          `json:"emails"`
	ByCategory map[string][]int `json:"-"`
	ByDomain   map[string][]int `json:"-"`
	Stats      EmailStats       `json:"stats"`
}

type EmailStats struct {
	Total      int            `json:"total"`
	Unread     int            `json:"unread"`
	ByCategory map[string]int `json:"by_category"`
	ByDomain   map[string]int `json:"by_domain"`
	TopSenders []SenderStat   `json:"top_senders"`
}

type SenderStat struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Count  int    `json:"count"`
}

// categorizeEmails runs keyword rules in parallel across CPU cores, then
// optionally enriches General-labelled emails via Ollama.
func categorizeEmails(emails []Email) CategorizeResult {
	if len(emails) == 0 {
		return buildResult(emails)
	}

	// Pass 1: parallel keyword categorisation.
	// Split the slice into numCPU chunks; each goroutine owns its chunk
	// exclusively so there is no write contention.
	numWorkers := runtime.NumCPU()
	if numWorkers > len(emails) {
		numWorkers = len(emails)
	}
	chunkSize := (len(emails) + numWorkers - 1) / numWorkers
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		start := w * chunkSize
		end := start + chunkSize
		if end > len(emails) {
			end = len(emails)
		}
		if start >= end {
			break
		}
		wg.Add(1)
		go func(slice []Email) {
			defer wg.Done()
			for i := range slice {
				slice[i].Category = categorizeOne(&slice[i])
			}
		}(emails[start:end])
	}
	wg.Wait()

	// Pass 2: Local AI enrichment for General-labelled emails.
	// Uses Naive Bayes trained on the keyword-labelled emails.
	domainCache := loadDomainCache()
	domainCache = enrichWithLocalAI(emails, domainCache)
	saveDomainCache(domainCache)

	// Re-label any General email whose domain was resolved.
	for i := range emails {
		if emails[i].Category == CatGeneral {
			if llmCat, ok := domainCache[emails[i].Domain]; ok && validCategories[llmCat] {
				emails[i].Category = llmCat
			}
		}
	}

	return buildResult(emails)
}

// rebuildFromCached reconstructs the full CategorizeResult from emails that
// already carry their Category field (e.g. loaded from disk cache).
// This skips keyword rules and LLM entirely.
func rebuildFromCached(emails []Email) CategorizeResult {
	for i := range emails {
		if emails[i].Category == "" {
			emails[i].Category = CatGeneral
		}
	}
	return buildResult(emails)
}

// buildResult assembles ByCategory and ByDomain index maps plus Stats.
// Indices point into the Emails slice — no Email structs are copied.
func buildResult(emails []Email) CategorizeResult {
	byCategory := make(map[string][]int, 10)
	byDomain := make(map[string][]int, 512)

	unread := 0
	for i := range emails {
		e := &emails[i]
		cat := e.Category
		if cat == "" {
			cat = CatGeneral
		}
		dom := e.Domain
		if dom == "" {
			dom = "unknown"
		}
		byCategory[cat] = append(byCategory[cat], i)
		byDomain[dom] = append(byDomain[dom], i)
		if e.Unread {
			unread++
		}
	}

	catCounts := make(map[string]int, len(byCategory))
	for k, v := range byCategory {
		catCounts[k] = len(v)
	}
	domCounts := make(map[string]int, len(byDomain))
	for k, v := range byDomain {
		domCounts[k] = len(v)
	}

	stats := EmailStats{
		Total:      len(emails),
		Unread:     unread,
		ByCategory: catCounts,
		ByDomain:   domCounts,
		TopSenders: topSenders(emails, 10),
	}

	return CategorizeResult{
		Emails:     emails,
		ByCategory: byCategory,
		ByDomain:   byDomain,
		Stats:      stats,
	}
}

func topSenders(emails []Email, n int) []SenderStat {
	type entry struct {
		count  int
		name   string
		domain string
	}
	counter := map[string]*entry{}
	for _, e := range emails {
		addr := e.SenderEmail
		if addr == "" {
			addr = "unknown"
		}
		if _, ok := counter[addr]; !ok {
			counter[addr] = &entry{}
		}
		counter[addr].count++
		if e.SenderName != "" {
			counter[addr].name = e.SenderName
		}
		if e.Domain != "" {
			counter[addr].domain = e.Domain
		}
	}

	type kv struct {
		addr string
		e    *entry
	}
	sorted := make([]kv, 0, len(counter))
	for k, v := range counter {
		sorted = append(sorted, kv{k, v})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].e.count > sorted[j].e.count
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}

	out := make([]SenderStat, 0, len(sorted))
	for _, kv := range sorted {
		out = append(out, SenderStat{
			Email:  kv.addr,
			Name:   kv.e.name,
			Domain: kv.e.domain,
			Count:  kv.e.count,
		})
	}
	return out
}
