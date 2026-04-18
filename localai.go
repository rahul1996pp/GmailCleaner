package main

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// ── Naive Bayes text classifier ───────────────────────────────────────────────
// Pure Go, zero external dependencies.  Trained on the keyword-labelled emails
// and used to enrich "General" leftovers without requiring Ollama.

type NaiveBayes struct {
	classCounts map[string]int
	wordCounts  map[string]map[string]int // word → class → count
	totalWords  map[string]int            // class → total words
	vocab       map[string]bool
	totalDocs   int
}

func NewNaiveBayes() *NaiveBayes {
	return &NaiveBayes{
		classCounts: make(map[string]int),
		wordCounts:  make(map[string]map[string]int),
		totalWords:  make(map[string]int),
		vocab:       make(map[string]bool),
	}
}

func (nb *NaiveBayes) Train(text, class string) {
	nb.classCounts[class]++
	nb.totalDocs++
	for _, word := range tokenize(text) {
		nb.vocab[word] = true
		if nb.wordCounts[word] == nil {
			nb.wordCounts[word] = make(map[string]int)
		}
		nb.wordCounts[word][class]++
		nb.totalWords[class]++
	}
}

// Predict returns the best class and a confidence in [0,1].
func (nb *NaiveBayes) Predict(text string) (string, float64) {
	if nb.totalDocs == 0 {
		return CatGeneral, 0
	}
	words := tokenize(text)
	vocabSize := float64(len(nb.vocab))
	if vocabSize == 0 {
		vocabSize = 1
	}

	type classScore struct {
		class string
		score float64
	}
	var scores []classScore

	for class, count := range nb.classCounts {
		logScore := math.Log(float64(count) / float64(nb.totalDocs))
		total := float64(nb.totalWords[class])
		for _, word := range words {
			wc := 0
			if nb.wordCounts[word] != nil {
				wc = nb.wordCounts[word][class]
			}
			logScore += math.Log((float64(wc) + 1.0) / (total + vocabSize))
		}
		scores = append(scores, classScore{class, logScore})
	}

	if len(scores) == 0 {
		return CatGeneral, 0
	}

	// Find max score for softmax-style confidence.
	sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
	best := scores[0]

	// Compute approximate confidence via log-sum-exp.
	maxLog := best.score
	sumExp := 0.0
	for _, s := range scores {
		sumExp += math.Exp(s.score - maxLog)
	}
	confidence := 1.0 / sumExp // probability of the best class
	if math.IsNaN(confidence) || math.IsInf(confidence, 0) {
		confidence = 0.5
	}

	return best.class, confidence
}

// ── Tokeniser ─────────────────────────────────────────────────────────────────

func tokenize(text string) []string {
	text = strings.ToLower(text)
	var result []string
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) > 1 && !stopWords[w] {
			result = append(result, w)
		}
	}
	return result
}

var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "shall": true, "should": true,
	"may": true, "might": true, "can": true, "could": true, "must": true,
	"to": true, "of": true, "in": true, "for": true, "on": true,
	"with": true, "at": true, "by": true, "from": true, "as": true,
	"into": true, "through": true, "during": true, "before": true,
	"after": true, "above": true, "below": true, "between": true,
	"and": true, "but": true, "or": true, "nor": true, "not": true,
	"so": true, "yet": true, "both": true, "either": true, "neither": true,
	"it": true, "its": true, "this": true, "that": true, "these": true,
	"those": true, "he": true, "she": true, "we": true, "they": true,
	"me": true, "him": true, "her": true, "us": true, "them": true,
	"my": true, "his": true, "our": true, "your": true, "their": true,
	"what": true, "which": true, "who": true, "whom": true, "whose": true,
	"where": true, "when": true, "why": true, "how": true,
	"if": true, "then": true, "else": true, "than": true,
	"no": true, "yes": true, "up": true, "out": true, "about": true,
	"just": true, "also": true, "very": true, "each": true, "all": true,
	"any": true, "few": true, "more": true, "most": true, "other": true,
	"some": true, "such": true, "only": true, "own": true, "same": true,
	"here": true, "there": true, "re": true, "ve": true, "ll": true,
	"you": true, "i": true, "am": true,
}

// ── Local AI enrichment (replaces Ollama) ────────────────────────────────────

// enrichWithLocalAI trains a Naive Bayes on already-categorised emails and
// uses it to predict categories for "General" emails.  Results are stored
// in the domain cache so they only need to be computed once per domain.
func enrichWithLocalAI(emails []Email, domainCache map[string]string) map[string]string {
	nb := NewNaiveBayes()

	for _, e := range emails {
		if e.Category == CatGeneral || e.Category == "" {
			continue
		}
		text := e.SenderEmail + " " + e.Domain + " " + e.Subject + " " + e.Snippet
		nb.Train(text, e.Category)
	}

	// Need enough training data for reasonable predictions.
	if nb.totalDocs < 50 {
		return domainCache
	}

	// Gather General emails grouped by domain.
	type domInfo struct {
		texts []string
	}
	domainTexts := map[string]*domInfo{}
	for _, e := range emails {
		if e.Category != CatGeneral && e.Category != "" {
			continue
		}
		dom := e.Domain
		if dom == "" {
			dom = "unknown"
		}
		if _, inCache := domainCache[dom]; inCache {
			continue
		}
		if domainTexts[dom] == nil {
			domainTexts[dom] = &domInfo{}
		}
		domainTexts[dom].texts = append(domainTexts[dom].texts,
			e.SenderEmail+" "+e.Subject+" "+e.Snippet)
	}

	if len(domainTexts) == 0 {
		return domainCache
	}

	updated := make(map[string]string, len(domainCache)+len(domainTexts))
	for k, v := range domainCache {
		updated[k] = v
	}

	for domain, info := range domainTexts {
		// Majority vote across all emails from this domain.
		votes := map[string]int{}
		totalConf := map[string]float64{}
		for _, text := range info.texts {
			cat, conf := nb.Predict(text)
			if conf > 0.25 {
				votes[cat]++
				totalConf[cat] += conf
			}
		}
		bestCat := ""
		bestScore := 0.0
		for cat, count := range votes {
			// Combine count with average confidence for ranking.
			score := float64(count) * (totalConf[cat] / float64(count))
			if score > bestScore {
				bestScore = score
				bestCat = cat
			}
		}
		// Require at least 2 votes or a single high-confidence vote.
		minVotes := 2
		if len(info.texts) == 1 {
			minVotes = 1
		}
		if bestCat != "" && votes[bestCat] >= minVotes && bestCat != CatGeneral {
			updated[domain] = bestCat
		}
	}

	return updated
}

// ── Enhanced rule-based Q&A (replaces Ollama Q&A) ────────────────────────────

func localAnswer(question string, emails []Email, stats EmailStats) AskResponse {
	q := strings.ToLower(question)

	attachCount := 0
	for _, e := range emails {
		if e.HasAttach {
			attachCount++
		}
	}

	switch {
	// ── Count queries ──
	case strings.Contains(q, "how many") && (strings.Contains(q, "email") || strings.Contains(q, "mail")):
		return AskResponse{Answer: "You have " + sitoa(stats.Total) + " emails total, " +
			sitoa(stats.Unread) + " unread, " + sitoa(attachCount) + " with attachments."}

	case strings.Contains(q, "how many") && strings.Contains(q, "spam"):
		sc := stats.ByCategory[CatSpam]
		return AskResponse{Answer: "You have " + sitoa(sc) + " spam emails. I recommend deleting all of them."}

	case strings.Contains(q, "how many") && strings.Contains(q, "unread"):
		return AskResponse{Answer: "You have " + sitoa(stats.Unread) + " unread emails out of " + sitoa(stats.Total) + " total."}

	case strings.Contains(q, "how many") && strings.Contains(q, "attachment"):
		return AskResponse{Answer: "You have " + sitoa(attachCount) + " emails with attachments."}

	case strings.Contains(q, "how many") && strings.Contains(q, "newsletter"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatNewsletter]) + " newsletter emails."}

	case strings.Contains(q, "how many") && strings.Contains(q, "promot"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatPromotional]) + " promotional emails."}

	case strings.Contains(q, "how many") && strings.Contains(q, "campaign"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatCampaign]) + " campaign emails."}

	case strings.Contains(q, "how many") && strings.Contains(q, "social"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatSocial]) + " social emails."}

	case strings.Contains(q, "how many") && strings.Contains(q, "notif"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatNotification]) + " notification emails."}

	case strings.Contains(q, "how many") && strings.Contains(q, "transact"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatTransactional]) + " transactional emails."}

	case strings.Contains(q, "how many") && strings.Contains(q, "domain"):
		return AskResponse{Answer: "You have emails from " + sitoa(len(stats.ByDomain)) + " different domains."}

	// ── Spam ──
	case strings.Contains(q, "spam"):
		sc := stats.ByCategory[CatSpam]
		return AskResponse{Answer: "You have " + sitoa(sc) + " spam emails. Select the Spam category in the sidebar and delete them all."}

	// ── Cleanup / delete suggestions ──
	case strings.Contains(q, "clean") || strings.Contains(q, "suggest") || strings.Contains(q, "delete") || strings.Contains(q, "remove"):
		suggestions := computeDeleteSuggestions(emails)
		if len(suggestions) > 0 {
			domCounts := map[string]int{}
			for _, s := range suggestions {
				domCounts[s.Domain]++
			}
			type dc struct {
				d string
				c int
			}
			var topDoms []dc
			for d, c := range domCounts {
				topDoms = append(topDoms, dc{d, c})
			}
			sort.Slice(topDoms, func(i, j int) bool { return topDoms[i].c > topDoms[j].c })
			limit := 10
			if len(topDoms) < limit {
				limit = len(topDoms)
			}
			var lines []string
			for _, d := range topDoms[:limit] {
				lines = append(lines, "  "+d.d+": "+sitoa(d.c)+" emails")
			}
			return AskResponse{Answer: "Based on your deletion history, I suggest reviewing " +
				sitoa(len(suggestions)) + " emails.\n\nTop domains to clean:\n" +
				strings.Join(lines, "\n") + "\n\nCheck the 'Suggested Delete' filter in the sidebar."}
		}
		var advice []string
		if stats.ByCategory[CatSpam] > 0 {
			advice = append(advice, "• Delete "+sitoa(stats.ByCategory[CatSpam])+" spam emails")
		}
		if stats.ByCategory[CatPromotional] > 50 {
			advice = append(advice, "• Review "+sitoa(stats.ByCategory[CatPromotional])+" promotional emails")
		}
		if stats.ByCategory[CatNewsletter] > 20 {
			advice = append(advice, "• Review "+sitoa(stats.ByCategory[CatNewsletter])+" newsletters — unsubscribe from ones you don't read")
		}
		if stats.ByCategory[CatCampaign] > 50 {
			advice = append(advice, "• Review "+sitoa(stats.ByCategory[CatCampaign])+" campaign emails")
		}

		// Find domains with many emails.
		type dc struct {
			d string
			c int
		}
		var bigDoms []dc
		for d, c := range stats.ByDomain {
			if c > 100 {
				bigDoms = append(bigDoms, dc{d, c})
			}
		}
		sort.Slice(bigDoms, func(i, j int) bool { return bigDoms[i].c > bigDoms[j].c })
		if len(bigDoms) > 5 {
			bigDoms = bigDoms[:5]
		}
		if len(bigDoms) > 0 {
			advice = append(advice, "\nDomains with 100+ emails:")
			for _, d := range bigDoms {
				advice = append(advice, "  "+d.d+": "+sitoa(d.c))
			}
		}
		if len(advice) == 0 {
			advice = append(advice, "Your inbox looks relatively clean!")
		}
		return AskResponse{Answer: "Cleanup suggestions:\n" + strings.Join(advice, "\n")}

	// ── Unread ──
	case strings.Contains(q, "unread"):
		return AskResponse{Answer: "You have " + sitoa(stats.Unread) + " unread emails out of " + sitoa(stats.Total) + " total (" +
			sitoa(int(float64(stats.Unread)/float64(max(stats.Total, 1))*100)) + "%)."}

	// ── Attachments ──
	case strings.Contains(q, "attachment"):
		return AskResponse{Answer: "You have " + sitoa(attachCount) + " emails with attachments out of " + sitoa(stats.Total) + " total."}

	// ── Newsletter ──
	case strings.Contains(q, "newsletter"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatNewsletter]) + " newsletter emails. Consider unsubscribing from ones you don't read regularly."}

	// ── Promotional ──
	case strings.Contains(q, "promot") || strings.Contains(q, "promo"):
		return AskResponse{Answer: "You have " + sitoa(stats.ByCategory[CatPromotional]) + " promotional emails. These are usually safe to delete in bulk."}

	// ── Top senders ──
	case strings.Contains(q, "top sender") || strings.Contains(q, "who send") || strings.Contains(q, "most email"):
		var lines []string
		for i, s := range stats.TopSenders {
			name := s.Name
			if name == "" {
				name = s.Email
			}
			lines = append(lines, sitoa(i+1)+". "+name+" ("+s.Domain+"): "+sitoa(s.Count)+" emails")
		}
		return AskResponse{Answer: "Top senders:\n" + strings.Join(lines, "\n")}

	// ── Top domains ──
	case strings.Contains(q, "domain") || strings.Contains(q, "top domain"):
		type dc struct {
			d string
			c int
		}
		var doms []dc
		for d, c := range stats.ByDomain {
			doms = append(doms, dc{d, c})
		}
		sort.Slice(doms, func(i, j int) bool { return doms[i].c > doms[j].c })
		limit := 15
		if len(doms) < limit {
			limit = len(doms)
		}
		var lines []string
		for i, d := range doms[:limit] {
			lines = append(lines, sitoa(i+1)+". "+d.d+": "+sitoa(d.c)+" emails")
		}
		return AskResponse{Answer: "Top " + sitoa(limit) + " domains:\n" + strings.Join(lines, "\n")}

	// ── Category breakdown ──
	case strings.Contains(q, "categor") || strings.Contains(q, "breakdown") || strings.Contains(q, "summary"):
		type cc struct {
			c string
			n int
		}
		var cats []cc
		for cat, count := range stats.ByCategory {
			cats = append(cats, cc{cat, count})
		}
		sort.Slice(cats, func(i, j int) bool { return cats[i].n > cats[j].n })
		var lines []string
		for _, c := range cats {
			pct := int(float64(c.n) / float64(max(stats.Total, 1)) * 100)
			lines = append(lines, "  "+c.c+": "+sitoa(c.n)+" ("+sitoa(pct)+"%)")
		}
		return AskResponse{Answer: "Category breakdown:\n" + strings.Join(lines, "\n") +
			"\n\nTotal: " + sitoa(stats.Total) + " emails"}

	// ── Deletion history / patterns ──
	case strings.Contains(q, "history") || strings.Contains(q, "pattern") || strings.Contains(q, "deleted"):
		deleteHistMu.RLock()
		hist := deleteHist
		deleteHistMu.RUnlock()
		if hist == nil || len(hist.Records) == 0 {
			return AskResponse{Answer: "No deletion history yet. Start deleting emails and I'll track patterns to make future suggestions."}
		}
		domDel := map[string]int{}
		catDel := map[string]int{}
		for _, r := range hist.Records {
			domDel[r.Domain]++
			catDel[r.Category]++
		}
		type dc struct {
			d string
			c int
		}
		var delDoms []dc
		for k, v := range domDel {
			delDoms = append(delDoms, dc{k, v})
		}
		sort.Slice(delDoms, func(i, j int) bool { return delDoms[i].c > delDoms[j].c })
		limit := 10
		if len(delDoms) < limit {
			limit = len(delDoms)
		}
		var lines []string
		lines = append(lines, "Total deleted: "+sitoa(len(hist.Records)))
		lines = append(lines, "\nTop deleted domains:")
		for _, d := range delDoms[:limit] {
			lines = append(lines, "  "+d.d+": "+sitoa(d.c))
		}
		lines = append(lines, "\nBy category:")
		for cat, c := range catDel {
			lines = append(lines, "  "+cat+": "+sitoa(c))
		}
		return AskResponse{Answer: strings.Join(lines, "\n")}

	// ── Search for specific senders/domains ──
	case strings.Contains(q, "from ") || strings.Contains(q, "find ") || strings.Contains(q, "search "):
		return searchEmails(q, emails, stats)

	// ── Tags ──
	case strings.Contains(q, "tag") || strings.Contains(q, "important") || strings.Contains(q, "keep"):
		tagSummary := getTagSummary()
		if len(tagSummary) == 0 {
			return AskResponse{Answer: "No tags set yet. You can tag emails as 'important', 'keep', 'spam', or create custom tags. Tagged emails with 'important' or 'keep' are protected from bulk deletion."}
		}
		var lines []string
		lines = append(lines, "Your email tags:")
		for tag, count := range tagSummary {
			lines = append(lines, "  "+tag+": "+sitoa(count)+" emails")
		}
		lines = append(lines, "\nEmails tagged 'important' or 'keep' are protected from deletion.")
		return AskResponse{Answer: strings.Join(lines, "\n")}

	// ── Accounts ──
	case strings.Contains(q, "account"):
		accounts := getAccountList()
		if len(accounts) == 0 {
			return AskResponse{Answer: "No accounts configured. Use the account switcher in the header to add accounts."}
		}
		var lines []string
		lines = append(lines, "Your accounts:")
		for _, a := range accounts {
			email, _ := a["email"].(string)
			active, _ := a["active"].(bool)
			marker := ""
			if active {
				marker = " (active)"
			}
			lines = append(lines, "  • "+email+marker)
		}
		return AskResponse{Answer: strings.Join(lines, "\n")}

	// ── Custom categories ──
	case strings.Contains(q, "custom") && strings.Contains(q, "categor"):
		customCats := getCustomCategories()
		if len(customCats) == 0 {
			return AskResponse{Answer: "No custom categories yet. Use the 'Move to Category' feature to create and assign custom categories."}
		}
		return AskResponse{Answer: "Custom categories: " + strings.Join(customCats, ", ")}

	// ── Oldest / newest ──
	case strings.Contains(q, "oldest") || strings.Contains(q, "first email"):
		if len(emails) == 0 {
			return AskResponse{Answer: "No emails loaded."}
		}
		oldest := emails[0]
		for _, e := range emails {
			if e.Date < oldest.Date {
				oldest = e
			}
		}
		return AskResponse{Answer: "Oldest email: From " + oldest.SenderName + " <" + oldest.SenderEmail + ">\nSubject: " + oldest.Subject + "\nDate: " + oldest.Date}

	case strings.Contains(q, "newest") || strings.Contains(q, "latest") || strings.Contains(q, "recent"):
		if len(emails) == 0 {
			return AskResponse{Answer: "No emails loaded."}
		}
		newest := emails[0]
		for _, e := range emails {
			if e.Date > newest.Date {
				newest = e
			}
		}
		return AskResponse{Answer: "Most recent email: From " + newest.SenderName + " <" + newest.SenderEmail + ">\nSubject: " + newest.Subject + "\nDate: " + newest.Date}

	// ── Biggest / largest senders by category ──
	case strings.Contains(q, "biggest") || strings.Contains(q, "largest"):
		type dc struct {
			d string
			c int
		}
		var doms []dc
		for d, c := range stats.ByDomain {
			doms = append(doms, dc{d, c})
		}
		sort.Slice(doms, func(i, j int) bool { return doms[i].c > doms[j].c })
		if len(doms) > 10 {
			doms = doms[:10]
		}
		var lines []string
		for _, d := range doms {
			lines = append(lines, d.d+": "+sitoa(d.c)+" emails")
		}
		return AskResponse{Answer: "Biggest email sources:\n" + strings.Join(lines, "\n")}

	// ── Default help ──
	default:
		return searchEmails(q, emails, stats)
	}

}

// searchEmails does a keyword search across all emails and returns a summary.
func searchEmails(query string, emails []Email, stats EmailStats) AskResponse {
	keywords := tokenize(query)
	// Remove question-like words
	var searchTerms []string
	qWords := map[string]bool{
		"show": true, "find": true, "search": true, "from": true,
		"get": true, "list": true, "tell": true, "about": true,
		"emails": true, "email": true, "mail": true, "mails": true,
		"many": true, "much": true, "what": true, "where": true,
	}
	for _, kw := range keywords {
		if !qWords[kw] {
			searchTerms = append(searchTerms, kw)
		}
	}
	if len(searchTerms) == 0 {
		return AskResponse{Answer: "I can help with:\n" +
			"• 'How many emails do I have?'\n" +
			"• 'Show top senders' / 'Show top domains'\n" +
			"• 'What should I clean up?'\n" +
			"• 'How many spam / newsletters / promotional?'\n" +
			"• 'Show category breakdown'\n" +
			"• 'How many unread / attachment emails?'\n" +
			"• 'Show deletion history / patterns'\n" +
			"• 'Find emails from [sender/domain]'\n" +
			"• 'Show oldest / newest email'\n" +
			"• 'Show my tags' / 'Show important emails'\n" +
			"• 'Which accounts do I have?'\n\n" +
			"Summary: " + sitoa(stats.Total) + " emails, " + sitoa(stats.Unread) + " unread across " + sitoa(len(stats.ByDomain)) + " domains."}
	}

	// Score emails by keyword match.
	type match struct {
		email Email
		score int
	}
	var matches []match
	for _, e := range emails {
		text := strings.ToLower(e.SenderName + " " + e.SenderEmail + " " + e.Domain + " " + e.Subject + " " + e.Snippet)
		score := 0
		for _, term := range searchTerms {
			if strings.Contains(text, term) {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, match{e, score})
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score > matches[j].score })

	if len(matches) == 0 {
		return AskResponse{Answer: "No emails found matching '" + strings.Join(searchTerms, " ") + "'."}
	}

	// Summarise results.
	total := len(matches)
	limit := 8
	if len(matches) < limit {
		limit = len(matches)
	}
	var lines []string
	lines = append(lines, "Found "+sitoa(total)+" emails matching '"+strings.Join(searchTerms, " ")+"':\n")
	for _, m := range matches[:limit] {
		name := m.email.SenderName
		if name == "" {
			name = m.email.SenderEmail
		}
		lines = append(lines, "• "+name+" — "+m.email.Subject+" ("+m.email.Date+")")
	}
	if total > limit {
		lines = append(lines, "\n...and "+sitoa(total-limit)+" more. Use the search bar to filter the full list.")
	}
	return AskResponse{Answer: strings.Join(lines, "\n")}
}

// ── AI retraining on delete ──────────────────────────────────────────────────
// When emails are deleted, we retrain the Naive Bayes model to learn what the
// user considers deletable. This is stored in the domain cache as a "Spam" or
// low-priority signal for similar future emails.

func retrainOnDelete(deletedEmails []Email, remainingEmails []Email) {
	nb := NewNaiveBayes()

	// Train on remaining emails as "keep" signals.
	for _, e := range remainingEmails {
		if e.Category == "" {
			continue
		}
		text := e.SenderEmail + " " + e.Domain + " " + e.Subject + " " + e.Snippet
		nb.Train(text, e.Category)
	}

	// Train deleted emails as negative signals — reinforce their categories
	// as "delete-worthy" so the model learns deletion patterns.
	for _, e := range deletedEmails {
		text := e.SenderEmail + " " + e.Domain + " " + e.Subject + " " + e.Snippet
		// If user deletes a "General" email, it's probably something unimportant.
		cat := e.Category
		if cat == "" || cat == CatGeneral {
			cat = CatPromotional // Reclassify deleted generals as promotional
		}
		nb.Train(text, cat)
	}

	if nb.totalDocs < 20 {
		return
	}

	// Update domain cache with insights from deletion patterns.
	domainCache := loadDomainCache()
	deletedDomains := map[string]int{}
	for _, e := range deletedEmails {
		deletedDomains[e.Domain]++
	}

	// If a domain was heavily deleted, mark it more aggressively.
	for domain, count := range deletedDomains {
		if count < 2 {
			continue
		}
		if _, alreadyCached := domainCache[domain]; alreadyCached {
			continue
		}
		// Use the Naive Bayes model to predict what category this domain should be.
		var texts []string
		for _, e := range deletedEmails {
			if e.Domain == domain {
				texts = append(texts, e.SenderEmail+" "+e.Subject+" "+e.Snippet)
			}
		}
		votes := map[string]int{}
		for _, text := range texts {
			cat, conf := nb.Predict(text)
			if conf > 0.2 && cat != CatGeneral {
				votes[cat]++
			}
		}
		bestCat := ""
		bestCount := 0
		for cat, c := range votes {
			if c > bestCount {
				bestCount = c
				bestCat = cat
			}
		}
		if bestCat != "" && bestCount >= 2 {
			domainCache[domain] = bestCat
		}
	}
	saveDomainCache(domainCache)
}
