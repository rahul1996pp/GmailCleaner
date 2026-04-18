package main

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Deletion history tracker ──────────────────────────────────────────────────

const deleteHistoryFile = "delete_history.json"

type DeleteRecord struct {
	Domain   string `json:"domain"`
	Sender   string `json:"sender"`
	Category string `json:"category"`
	Label    string `json:"label"`
	Time     string `json:"time"`
}

type DeleteHistory struct {
	Records []DeleteRecord `json:"records"`
}

type DeleteSuggestion struct {
	EmailID  string  `json:"email_id"`
	Score    float64 `json:"score"`
	Reason   string  `json:"reason"`
	Domain   string  `json:"domain"`
	Sender   string  `json:"sender"`
	Category string  `json:"category"`
}

var (
	deleteHistMu sync.RWMutex
	deleteHist   *DeleteHistory
)

func deleteHistoryPath() string {
	return dataDir + "/" + deleteHistoryFile
}

func loadDeleteHistory() *DeleteHistory {
	data, err := os.ReadFile(deleteHistoryPath())
	if err != nil {
		return &DeleteHistory{}
	}
	var h DeleteHistory
	if err := json.Unmarshal(data, &h); err != nil {
		return &DeleteHistory{}
	}
	return &h
}

func saveDeleteHistory(h *DeleteHistory) {
	_ = os.MkdirAll(dataDir, 0700)
	data, _ := json.MarshalIndent(h, "", "  ")
	_ = os.WriteFile(deleteHistoryPath(), data, 0644)
}

func initDeleteHistory() {
	deleteHistMu.Lock()
	defer deleteHistMu.Unlock()
	deleteHist = loadDeleteHistory()
}

// recordDeletions logs the patterns of deleted emails.
func recordDeletions(emails []Email) {
	deleteHistMu.Lock()
	defer deleteHistMu.Unlock()

	if deleteHist == nil {
		deleteHist = loadDeleteHistory()
	}

	now := time.Now().Format(time.RFC3339)
	for _, e := range emails {
		label := e.Category + ":" + e.Domain
		deleteHist.Records = append(deleteHist.Records, DeleteRecord{
			Domain:   e.Domain,
			Sender:   e.SenderEmail,
			Category: e.Category,
			Label:    label,
			Time:     now,
		})
	}
	saveDeleteHistory(deleteHist)
}

// computeDeleteSuggestions scores emails based on deletion history.
func computeDeleteSuggestions(emails []Email) []DeleteSuggestion {
	deleteHistMu.RLock()
	hist := deleteHist
	deleteHistMu.RUnlock()

	if hist == nil || len(hist.Records) == 0 {
		return nil
	}

	domainCount := map[string]int{}
	senderCount := map[string]int{}
	categoryCount := map[string]int{}
	labelCount := map[string]int{}
	total := float64(len(hist.Records))

	for _, r := range hist.Records {
		domainCount[r.Domain]++
		senderCount[r.Sender]++
		categoryCount[r.Category]++
		labelCount[r.Label]++
	}

	type scored struct {
		idx    int
		score  float64
		reason string
	}
	var results []scored

	for i, e := range emails {
		score := 0.0
		var reasons []string

		if dc := domainCount[e.Domain]; dc > 0 {
			score += math.Min(float64(dc)/total*5, 1.0) * 0.40
			reasons = append(reasons, "domain frequently deleted")
		}
		if sc := senderCount[e.SenderEmail]; sc > 0 {
			score += math.Min(float64(sc)/total*8, 1.0) * 0.30
			reasons = append(reasons, "sender frequently deleted")
		}
		if cc := categoryCount[e.Category]; cc > 0 {
			score += math.Min(float64(cc)/total*3, 1.0) * 0.15
			reasons = append(reasons, e.Category+" often deleted")
		}
		label := e.Category + ":" + e.Domain
		if lc := labelCount[label]; lc > 0 {
			score += math.Min(float64(lc)/total*6, 1.0) * 0.15
			reasons = append(reasons, e.Category+"+"+e.Domain+" pattern")
		}

		if score > 0.05 {
			results = append(results, scored{i, math.Min(score, 1.0), strings.Join(reasons, "; ")})
		}
	}

	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })

	suggestions := make([]DeleteSuggestion, 0, len(results))
	for _, r := range results {
		e := emails[r.idx]
		suggestions = append(suggestions, DeleteSuggestion{
			EmailID:  e.ID,
			Score:    math.Round(r.score*100) / 100,
			Reason:   r.reason,
			Domain:   e.Domain,
			Sender:   e.SenderEmail,
			Category: e.Category,
		})
	}
	return suggestions
}

// ── ML-style confidence scoring ───────────────────────────────────────────────

type CategoryScore struct {
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
}

func scoreEmail(e *Email) []CategoryScore {
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

	scores := map[string]float64{
		CatNewsletter:    0,
		CatPromotional:   0,
		CatSocial:        0,
		CatNotification:  0,
		CatTransactional: 0,
		CatCampaign:      0,
		CatSpam:          0,
		CatGeneral:       0.1,
	}

	if gmailLabel == "spam" {
		scores[CatSpam] += 0.9
	}
	if knownSpamDomains[domain] {
		scores[CatSpam] += 0.8
	}

	countMatches := func(text string, keywords []string) float64 {
		count := 0
		for _, kw := range keywords {
			if strings.Contains(text, kw) {
				count++
			}
		}
		return float64(count)
	}

	if n := countMatches(combined, spamKeywords); n > 0 {
		scores[CatSpam] += math.Min(n*0.15, 0.7)
	}
	if n := countMatches(combined, promotionalKeywords); n > 0 {
		scores[CatPromotional] += math.Min(n*0.12, 0.8)
	}
	if n := countMatches(combined, newsletterKeywords); n > 0 {
		scores[CatNewsletter] += math.Min(n*0.15, 0.8)
	}
	if matchesAny(senderLocal, []string{"newsletter", "digest", "news"}) {
		scores[CatNewsletter] += 0.3
	}
	if socialDomains[domain] {
		scores[CatSocial] += 0.7
	}
	if n := countMatches(combined, socialKeywords); n > 0 {
		scores[CatSocial] += math.Min(n*0.15, 0.5)
	}
	if n := countMatches(combined, transactionalKeywords); n > 0 {
		scores[CatTransactional] += math.Min(n*0.15, 0.8)
	}
	if n := countMatches(combined, notificationKeywords); n > 0 {
		scores[CatNotification] += math.Min(n*0.10, 0.6)
	}
	if matchesAny(senderLocal, campaignSenders) {
		scores[CatCampaign] += 0.4
	}

	var result []CategoryScore
	maxScore := 0.0
	for _, s := range scores {
		if s > maxScore {
			maxScore = s
		}
	}
	if maxScore == 0 {
		maxScore = 1
	}
	for cat, s := range scores {
		if s > 0.01 {
			result = append(result, CategoryScore{
				Category:   cat,
				Confidence: math.Round(s/maxScore*100) / 100,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Confidence > result[j].Confidence })
	return result
}

// ── Label+Domain combos ──────────────────────────────────────────────────────

type LabelDomainGroup struct {
	Label    string `json:"label"`
	Category string `json:"category"`
	Domain   string `json:"domain"`
	Count    int    `json:"count"`
}

func buildLabelDomainGroups(emails []Email) []LabelDomainGroup {
	type key struct{ cat, dom string }
	groupMap := map[key]*LabelDomainGroup{}

	for _, e := range emails {
		cat := e.Category
		if cat == "" {
			cat = CatGeneral
		}
		dom := e.Domain
		if dom == "" {
			dom = "unknown"
		}
		k := key{cat, dom}
		if g, ok := groupMap[k]; ok {
			g.Count++
		} else {
			groupMap[k] = &LabelDomainGroup{
				Label:    cat + ":" + dom,
				Category: cat,
				Domain:   dom,
				Count:    1,
			}
		}
	}

	groups := make([]LabelDomainGroup, 0, len(groupMap))
	for _, g := range groupMap {
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Count > groups[j].Count })
	return groups
}

// ── AI Q&A (pure Go, no external dependencies) ───────────────────────────────

type AskRequest struct {
	Question string `json:"question"`
}

type AskResponse struct {
	Answer string `json:"answer"`
	Error  string `json:"error,omitempty"`
}

// askAI answers user questions about their email data using the local
// rule-based + Naive-Bayes powered engine.  No Ollama or external LLM needed.
func askAI(question string, emails []Email, stats EmailStats) AskResponse {
	return localAnswer(question, emails, stats)
}

// sitoa is a simple int-to-string helper.
func sitoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	d := ""
	for n > 0 {
		d = string(rune('0'+n%10)) + d
		n /= 10
	}
	if neg {
		d = "-" + d
	}
	return d
}
