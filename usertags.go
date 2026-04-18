package main

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// ── User tags & custom categories ─────────────────────────────────────────────
// Tags: "important", "spam", "keep", or user-created.
// Tagged emails are protected during bulk-delete ("important"/"keep" block deletion).
// Custom categories: user-created categories that extend the built-in ones.
// All data persists in browser_data/user_tags.json.

const userTagsFile = "user_tags.json"

// TagData is the on-disk format.
type TagData struct {
	// EmailID → set of tags
	Tags map[string][]string `json:"tags"`
	// Custom category names created by user
	CustomCategories []string `json:"custom_categories"`
	// EmailID → custom category override (takes priority over auto-categorise)
	CategoryOverrides map[string]string `json:"category_overrides"`
}

var (
	tagMu   sync.RWMutex
	tagData *TagData
)

func tagDataPath() string {
	return dataDir + "/" + userTagsFile
}

func loadTagData() *TagData {
	data, err := os.ReadFile(tagDataPath())
	if err != nil {
		return &TagData{
			Tags:              make(map[string][]string),
			CustomCategories:  []string{},
			CategoryOverrides: make(map[string]string),
		}
	}
	var td TagData
	if err := json.Unmarshal(data, &td); err != nil {
		return &TagData{
			Tags:              make(map[string][]string),
			CustomCategories:  []string{},
			CategoryOverrides: make(map[string]string),
		}
	}
	if td.Tags == nil {
		td.Tags = make(map[string][]string)
	}
	if td.CategoryOverrides == nil {
		td.CategoryOverrides = make(map[string]string)
	}
	return &td
}

func saveTagData(td *TagData) {
	_ = os.MkdirAll(dataDir, 0700)
	data, _ := json.MarshalIndent(td, "", "  ")
	_ = os.WriteFile(tagDataPath(), data, 0644)
}

func initTagData() {
	tagMu.Lock()
	defer tagMu.Unlock()
	tagData = loadTagData()
}

// ── Tag operations ────────────────────────────────────────────────────────────

// setTags replaces the tag set for given email IDs.
func setTags(emailIDs []string, tags []string) {
	tagMu.Lock()
	defer tagMu.Unlock()
	for _, id := range emailIDs {
		if len(tags) == 0 {
			delete(tagData.Tags, id)
		} else {
			tagData.Tags[id] = tags
		}
	}
	saveTagData(tagData)
}

// addTag adds a single tag to multiple emails without removing existing tags.
func addTag(emailIDs []string, tag string) {
	tagMu.Lock()
	defer tagMu.Unlock()
	for _, id := range emailIDs {
		existing := tagData.Tags[id]
		found := false
		for _, t := range existing {
			if t == tag {
				found = true
				break
			}
		}
		if !found {
			tagData.Tags[id] = append(existing, tag)
		}
	}
	saveTagData(tagData)
}

// removeTag removes a single tag from multiple emails.
func removeTag(emailIDs []string, tag string) {
	tagMu.Lock()
	defer tagMu.Unlock()
	for _, id := range emailIDs {
		existing := tagData.Tags[id]
		var updated []string
		for _, t := range existing {
			if t != tag {
				updated = append(updated, t)
			}
		}
		if len(updated) == 0 {
			delete(tagData.Tags, id)
		} else {
			tagData.Tags[id] = updated
		}
	}
	saveTagData(tagData)
}

// getEmailTags returns the tags for a single email.
func getEmailTags(emailID string) []string {
	tagMu.RLock()
	defer tagMu.RUnlock()
	return tagData.Tags[emailID]
}

// getAllTags returns all unique tags in use.
func getAllTags() []string {
	tagMu.RLock()
	defer tagMu.RUnlock()
	seen := map[string]bool{}
	for _, tags := range tagData.Tags {
		for _, t := range tags {
			seen[t] = true
		}
	}
	// Always include defaults
	seen["important"] = true
	seen["keep"] = true
	seen["spam"] = true
	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	sort.Strings(result)
	return result
}

// getTaggedEmailIDs returns all email IDs that have a specific tag.
func getTaggedEmailIDs(tag string) map[string]bool {
	tagMu.RLock()
	defer tagMu.RUnlock()
	ids := map[string]bool{}
	for id, tags := range tagData.Tags {
		for _, t := range tags {
			if t == tag {
				ids[id] = true
				break
			}
		}
	}
	return ids
}

// isProtected checks if an email has "important" or "keep" tags.
func isProtected(emailID string) bool {
	tagMu.RLock()
	defer tagMu.RUnlock()
	for _, t := range tagData.Tags[emailID] {
		if t == "important" || t == "keep" {
			return true
		}
	}
	return false
}

// filterProtected returns only the IDs that are NOT protected.
func filterProtected(emailIDs []string) (allowed []string, blocked []string) {
	tagMu.RLock()
	defer tagMu.RUnlock()
	for _, id := range emailIDs {
		prot := false
		for _, t := range tagData.Tags[id] {
			if t == "important" || t == "keep" {
				prot = true
				break
			}
		}
		if prot {
			blocked = append(blocked, id)
		} else {
			allowed = append(allowed, id)
		}
	}
	return
}

// getTagSummary returns tag → count for the UI.
func getTagSummary() map[string]int {
	tagMu.RLock()
	defer tagMu.RUnlock()
	summary := map[string]int{}
	for _, tags := range tagData.Tags {
		for _, t := range tags {
			summary[t]++
		}
	}
	return summary
}

// ── Custom categories ─────────────────────────────────────────────────────────

func addCustomCategory(name string) bool {
	tagMu.Lock()
	defer tagMu.Unlock()
	// Check for duplicate (case-insensitive against builtins)
	for _, c := range tagData.CustomCategories {
		if c == name {
			return false
		}
	}
	if validCategories[name] {
		return false // built-in
	}
	tagData.CustomCategories = append(tagData.CustomCategories, name)
	saveTagData(tagData)
	return true
}

func getCustomCategories() []string {
	tagMu.RLock()
	defer tagMu.RUnlock()
	return tagData.CustomCategories
}

func getAllCategories() []string {
	tagMu.RLock()
	defer tagMu.RUnlock()
	cats := []string{CatNewsletter, CatPromotional, CatSocial, CatNotification,
		CatTransactional, CatCampaign, CatSpam, CatGeneral}
	cats = append(cats, tagData.CustomCategories...)
	return cats
}

// moveToCategory sets a custom category override for emails.
func moveToCategory(emailIDs []string, category string) {
	tagMu.Lock()
	defer tagMu.Unlock()
	for _, id := range emailIDs {
		tagData.CategoryOverrides[id] = category
	}
	saveTagData(tagData)
}

// getCategoryOverrides returns the full override map.
func getCategoryOverrides() map[string]string {
	tagMu.RLock()
	defer tagMu.RUnlock()
	result := make(map[string]string, len(tagData.CategoryOverrides))
	for k, v := range tagData.CategoryOverrides {
		result[k] = v
	}
	return result
}

// applyCategoryOverrides applies user overrides to email slice in-place.
func applyCategoryOverrides(emails []Email) {
	overrides := getCategoryOverrides()
	if len(overrides) == 0 {
		return
	}
	for i := range emails {
		if cat, ok := overrides[emails[i].ID]; ok {
			emails[i].Category = cat
		}
	}
}

// getTagMap returns the full emailID→tags map for sending to the UI.
func getTagMap() map[string][]string {
	tagMu.RLock()
	defer tagMu.RUnlock()
	result := make(map[string][]string, len(tagData.Tags))
	for k, v := range tagData.Tags {
		result[k] = v
	}
	return result
}
