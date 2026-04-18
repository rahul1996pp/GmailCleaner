package main

import (
	"fmt"
	"strings"
	"testing"
)

// ── categorizeOne ─────────────────────────────────────────────────────────────

func TestCategorizeOne_Spam(t *testing.T) {
	cases := []struct {
		name  string
		email Email
	}{
		{"gmail spam label", Email{GmailLabel: "Spam", Subject: "hello"}},
		{"known spam domain", Email{Domain: "mailinator.com", Subject: "hi"}},
		{"spam keyword subject", Email{Subject: "You have won a lottery prize!"}},
		{"spam keyword snippet", Email{Subject: "hi", Snippet: "claim your prize now"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := categorizeOne(&tc.email); got != CatSpam {
				t.Errorf("want Spam, got %q", got)
			}
		})
	}
}

func TestCategorizeOne_Promotional(t *testing.T) {
	cases := []Email{
		{Subject: "50% off your next order!"},
		{Subject: "Flash sale ends tonight", Snippet: "limited time offer"},
		{Subject: "Free shipping on all orders today only"},
	}
	for _, e := range cases {
		t.Run(e.Subject, func(t *testing.T) {
			if got := categorizeOne(&e); got != CatPromotional {
				t.Errorf("want Promotional, got %q for %q", got, e.Subject)
			}
		})
	}
}

func TestCategorizeOne_Newsletter(t *testing.T) {
	cases := []Email{
		{Subject: "The Weekly Digest Issue #42"},
		{Subject: "Morning Brief: top stories"},
		{SenderEmail: "newsletter@example.com", Subject: "Company updates"},
		{SenderEmail: "digest@news.co", Subject: "Your weekly roundup"},
	}
	for _, e := range cases {
		t.Run(e.Subject+e.SenderEmail, func(t *testing.T) {
			if got := categorizeOne(&e); got != CatNewsletter {
				t.Errorf("want Newsletter, got %q", got)
			}
		})
	}
}

func TestCategorizeOne_Social(t *testing.T) {
	cases := []Email{
		{Domain: "linkedin.com", Subject: "Someone viewed your profile"},
		{Domain: "facebookmail.com", Subject: "You have a new notification"},
		{Subject: "Alice liked your post"},
		{Subject: "Bob sent you a message"},
	}
	for _, e := range cases {
		t.Run(e.Subject+e.Domain, func(t *testing.T) {
			if got := categorizeOne(&e); got != CatSocial {
				t.Errorf("want Social, got %q for %+v", got, e)
			}
		})
	}
}

func TestCategorizeOne_Transactional(t *testing.T) {
	cases := []Email{
		{Subject: "Your order confirmation #12345"},
		{Subject: "Invoice from Acme Corp"},
		{Subject: "Payment received thank you"},
		{Subject: "Refund processed for your recent purchase"},
	}
	for _, e := range cases {
		t.Run(e.Subject, func(t *testing.T) {
			if got := categorizeOne(&e); got != CatTransactional {
				t.Errorf("want Transactional, got %q", got)
			}
		})
	}
}

func TestCategorizeOne_Notification(t *testing.T) {
	cases := []Email{
		{Subject: "Security alert: new login from Chrome"},
		{Subject: "Action required: verify your account"},
		{Subject: "Password reset requested"},
	}
	for _, e := range cases {
		t.Run(e.Subject, func(t *testing.T) {
			got := categorizeOne(&e)
			if got != CatNotification && got != CatTransactional {
				t.Errorf("want Notification or Transactional, got %q for %q", got, e.Subject)
			}
		})
	}
}

func TestCategorizeOne_Campaign(t *testing.T) {
	cases := []Email{
		{SenderEmail: "noreply@example.com", Subject: "A message from us"},
		{SenderEmail: "marketing@company.io", Subject: "Check this out"},
		{SenderEmail: "no-reply@service.net", Subject: "Hello from us"},
	}
	for _, e := range cases {
		t.Run(e.SenderEmail, func(t *testing.T) {
			if got := categorizeOne(&e); got != CatCampaign {
				t.Errorf("want Campaign, got %q for %q / %q", got, e.SenderEmail, e.Subject)
			}
		})
	}
}

func TestCategorizeOne_General(t *testing.T) {
	e := Email{
		SenderEmail: "bob@randomcompany.com",
		Subject:     "Hey catching up",
		Snippet:     "Long time no see how are you doing?",
		Domain:      "randomcompany.com",
	}
	if got := categorizeOne(&e); got != CatGeneral {
		t.Errorf("want General, got %q", got)
	}
}

func TestCategorizeOne_SpamWinsOverPromotional(t *testing.T) {
	e := Email{Subject: "50% OFF You have won a lottery prize!"}
	if got := categorizeOne(&e); got != CatSpam {
		t.Errorf("want Spam priority over Promotional, got %q", got)
	}
}

// ── categorizeEmails ──────────────────────────────────────────────────────────

func TestCategorizeEmails_Counts(t *testing.T) {
	emails := []Email{
		{ID: "imap:all:1", Subject: "50% off today only", Domain: "store.com"},
		{ID: "imap:all:2", Subject: "Weekly Digest #10", SenderEmail: "digest@news.org", Domain: "news.org"},
		{ID: "imap:spam:3", GmailLabel: "Spam", Subject: "You won!", Domain: "scam.com"},
		{ID: "imap:all:4", Subject: "Hey want to catch up?", SenderEmail: "friend@gmail.com", Domain: "gmail.com"},
	}
	r := categorizeEmails(emails)
	if r.Stats.Total != 4 {
		t.Errorf("Total: want 4, got %d", r.Stats.Total)
	}
	if r.Stats.ByCategory[CatPromotional] != 1 {
		t.Errorf("Promotional: want 1, got %d", r.Stats.ByCategory[CatPromotional])
	}
	if r.Stats.ByCategory[CatNewsletter] != 1 {
		t.Errorf("Newsletter: want 1, got %d", r.Stats.ByCategory[CatNewsletter])
	}
	if r.Stats.ByCategory[CatSpam] != 1 {
		t.Errorf("Spam: want 1, got %d", r.Stats.ByCategory[CatSpam])
	}
}

func TestCategorizeEmails_UnreadCount(t *testing.T) {
	emails := []Email{
		{ID: "imap:all:1", Unread: true, Subject: "hi"},
		{ID: "imap:all:2", Unread: false, Subject: "hi"},
		{ID: "imap:all:3", Unread: true, Subject: "hi"},
	}
	r := categorizeEmails(emails)
	if r.Stats.Unread != 2 {
		t.Errorf("Unread: want 2, got %d", r.Stats.Unread)
	}
}

// ── rebuildFromCached ─────────────────────────────────────────────────────────

func TestRebuildFromCached_PreservesCategories(t *testing.T) {
	emails := []Email{
		{ID: "imap:all:1", Category: CatNewsletter, Domain: "news.org"},
		{ID: "imap:all:2", Category: CatSpam, Domain: "scam.com"},
		{ID: "imap:all:3", Domain: "unknown.com"}, // no category -> General
	}
	r := rebuildFromCached(emails)
	if r.Stats.Total != 3 {
		t.Errorf("Total: want 3, got %d", r.Stats.Total)
	}
	if r.Stats.ByCategory[CatNewsletter] != 1 {
		t.Errorf("Newsletter: want 1, got %d", r.Stats.ByCategory[CatNewsletter])
	}
	if r.Stats.ByCategory[CatGeneral] != 1 {
		t.Errorf("empty category should become General, got %d", r.Stats.ByCategory[CatGeneral])
	}
}

func TestRebuildFromCached_Nil(t *testing.T) {
	r := rebuildFromCached(nil)
	if r.Stats.Total != 0 {
		t.Errorf("want 0 for nil input, got %d", r.Stats.Total)
	}
}

// ── topSenders ────────────────────────────────────────────────────────────────

func TestTopSenders_Order(t *testing.T) {
	emails := []Email{
		{SenderEmail: "a@x.com", SenderName: "Alice", Domain: "x.com"},
		{SenderEmail: "a@x.com", SenderName: "Alice", Domain: "x.com"},
		{SenderEmail: "a@x.com", SenderName: "Alice", Domain: "x.com"},
		{SenderEmail: "b@y.com", SenderName: "Bob", Domain: "y.com"},
		{SenderEmail: "b@y.com", SenderName: "Bob", Domain: "y.com"},
		{SenderEmail: "c@z.com", SenderName: "Carol", Domain: "z.com"},
	}
	top := topSenders(emails, 2)
	if len(top) != 2 {
		t.Fatalf("want 2, got %d", len(top))
	}
	if top[0].Email != "a@x.com" || top[0].Count != 3 {
		t.Errorf("first: want a@x.com/3, got %s/%d", top[0].Email, top[0].Count)
	}
	if top[1].Email != "b@y.com" || top[1].Count != 2 {
		t.Errorf("second: want b@y.com/2, got %s/%d", top[1].Email, top[1].Count)
	}
}

func TestTopSenders_LimitRespected(t *testing.T) {
	emails := make([]Email, 20)
	for i := range emails {
		emails[i] = Email{SenderEmail: fmt.Sprintf("u%d@x.com", i)}
	}
	if got := topSenders(emails, 5); len(got) != 5 {
		t.Errorf("want 5, got %d", len(got))
	}
}

// ── decodeHeader ──────────────────────────────────────────────────────────────

func TestDecodeHeader_Plain(t *testing.T) {
	if got := decodeHeader("Hello World"); got != "Hello World" {
		t.Errorf("got %q", got)
	}
}

func TestDecodeHeader_Empty(t *testing.T) {
	if got := decodeHeader(""); got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestDecodeHeader_QP(t *testing.T) {
	if got := decodeHeader("=?UTF-8?Q?Hello_World?="); got != "Hello World" {
		t.Errorf("want 'Hello World', got %q", got)
	}
}

func TestDecodeHeader_Base64(t *testing.T) {
	if got := decodeHeader("=?UTF-8?B?VGVzdA==?="); got != "Test" {
		t.Errorf("want 'Test', got %q", got)
	}
}

// ── parseFrom ────────────────────────────────────────────────────────────────

func TestParseFrom_FullName(t *testing.T) {
	name, addr := parseFrom(`"Alice Smith" <alice@example.com>`)
	if name != "Alice Smith" {
		t.Errorf("name: got %q", name)
	}
	if addr != "alice@example.com" {
		t.Errorf("addr: got %q", addr)
	}
}

func TestParseFrom_NoName(t *testing.T) {
	_, addr := parseFrom("<bob@example.org>")
	if addr != "bob@example.org" {
		t.Errorf("addr: got %q", addr)
	}
}

func TestParseFrom_BareAddr(t *testing.T) {
	_, addr := parseFrom("carol@example.net")
	if addr != "carol@example.net" {
		t.Errorf("addr: got %q", addr)
	}
}

func TestParseFrom_Empty(t *testing.T) {
	name, addr := parseFrom("")
	if name != "" || addr != "" {
		t.Errorf("want empty, got name=%q addr=%q", name, addr)
	}
}

// ── extractDomain ─────────────────────────────────────────────────────────────

func TestExtractDomain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"alice@mail.google.com", "google.com"},
		{"bob@example.com", "example.com"},
		{"x@sub.domain.co.uk", "co.uk"},
		{"noemail", "unknown"},
		{"", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := extractDomain(tc.in); got != tc.want {
				t.Errorf("extractDomain(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ── cleanSnippet ──────────────────────────────────────────────────────────────

func TestCleanSnippet_TruncatesAt200Runes(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "あ"
	}
	if got := []rune(cleanSnippet(long)); len(got) > 200 {
		t.Errorf("exceeds 200 runes: got %d", len(got))
	}
}

func TestCleanSnippet_CollapseWhitespace(t *testing.T) {
	if got := cleanSnippet("hello   \t  world"); got != "hello world" {
		t.Errorf("want 'hello world', got %q", got)
	}
}

func TestCleanSnippet_InvalidUTF8(t *testing.T) {
	got := cleanSnippet("hello \xff world")
	if got == "" {
		t.Error("want non-empty output for invalid UTF-8 input")
	}
}

// ── stripHTML ─────────────────────────────────────────────────────────────────

func TestStripHTML_Tags(t *testing.T) {
	if got := stripHTML("<p>Hello <b>World</b></p>"); got != "Hello World" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTML_ScriptRemoved(t *testing.T) {
	if got := stripHTML("<html><body><script>alert(1)</script>Safe</body></html>"); got != "Safe" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTML_StyleRemoved(t *testing.T) {
	if got := stripHTML("<style>body{color:red}</style>Content"); got != "Content" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTML_Entities(t *testing.T) {
	if got := stripHTML("Hello &amp; World &lt;3&gt;"); got != "Hello & World <3>" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTML_BlockTagsNewline(t *testing.T) {
	if got := stripHTML("<div>Line1</div><div>Line2</div>"); got != "Line1\nLine2" {
		t.Errorf("got %q", got)
	}
}

// ── isOK ─────────────────────────────────────────────────────────────────────

func TestIsOK(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"T0001 OK Success", true},
		{"T0002 OK [READ-ONLY] EXAMINE completed", true},
		{"T0003 ok lowercase", true},
		{"T0004 NO Not found", false},
		{"T0005 BAD Command error", false},
		{"T0006 BYE Server closing", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			if got := isOK(tc.line); got != tc.want {
				t.Errorf("isOK(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// ── set utilities ─────────────────────────────────────────────────────────────

func TestSetDiff(t *testing.T) {
	a := map[string]bool{"1": true, "2": true, "3": true}
	b := map[string]bool{"2": true, "3": true, "4": true}
	diff := setDiff(a, b)
	if len(diff) != 1 || !diff["1"] {
		t.Errorf("setDiff wrong: %v", diff)
	}
}

func TestSetDiff_Empty(t *testing.T) {
	if diff := setDiff(map[string]bool{}, map[string]bool{"a": true}); len(diff) != 0 {
		t.Errorf("expected empty, got %v", diff)
	}
}

func TestSliceToSet_Deduplicates(t *testing.T) {
	s := sliceToSet([]string{"a", "b", "a", "c", "b"})
	if len(s) != 3 {
		t.Errorf("expected 3, got %d: %v", len(s), s)
	}
}

func TestSliceToSet_Nil(t *testing.T) {
	if s := sliceToSet(nil); len(s) != 0 {
		t.Errorf("expected empty for nil input")
	}
}

func TestSetToSlice_RoundTrip(t *testing.T) {
	orig := map[string]bool{"x": true, "y": true, "z": true}
	back := sliceToSet(setToSlice(orig))
	for k := range orig {
		if !back[k] {
			t.Errorf("key %q lost in round-trip", k)
		}
	}
}

// ── parseSingleFetch ──────────────────────────────────────────────────────────

func TestParseSingleFetch_Basic(t *testing.T) {
	hdr := "From: Alice <alice@example.com>\r\nSubject: Test Email\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\n"
	snip := "This is the preview text."
	lines := []string{
		fmt.Sprintf(`* 1 FETCH (UID 42 FLAGS (\Seen) BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}`, len(hdr)),
		hdr,
		fmt.Sprintf(` BODY[TEXT]<0> {%d}`, len(snip)),
		snip,
		`)`,
	}
	e := parseSingleFetch(lines, "all", "All Mail")
	if e == nil {
		t.Fatal("nil result")
	}
	if e.ID != "imap:all:42" {
		t.Errorf("ID: got %q", e.ID)
	}
	if e.SenderEmail != "alice@example.com" {
		t.Errorf("SenderEmail: got %q", e.SenderEmail)
	}
	if e.Subject != "Test Email" {
		t.Errorf("Subject: got %q", e.Subject)
	}
	if e.Unread {
		t.Error("want Unread=false (has \\Seen)")
	}
	if e.Domain != "example.com" {
		t.Errorf("Domain: got %q", e.Domain)
	}
}

func TestParseSingleFetch_UnreadEmail(t *testing.T) {
	hdr := "Subject: X\r\n"
	lines := []string{
		fmt.Sprintf(`* 5 FETCH (UID 99 FLAGS () BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}`, len(hdr)),
		hdr, `)`,
	}
	e := parseSingleFetch(lines, "all", "All Mail")
	if e == nil {
		t.Fatal("nil result")
	}
	if !e.Unread {
		t.Error("want Unread=true when FLAGS empty")
	}
}

func TestParseSingleFetch_NoUID(t *testing.T) {
	if e := parseSingleFetch([]string{`* 1 FETCH (FLAGS (\Seen))`}, "all", "All Mail"); e != nil {
		t.Errorf("want nil for missing UID, got %+v", e)
	}
}

func TestParseSingleFetch_Empty(t *testing.T) {
	if e := parseSingleFetch(nil, "all", "All Mail"); e != nil {
		t.Error("want nil for empty lines")
	}
}

func TestParseSingleFetch_SpamFolder(t *testing.T) {
	hdr := "From: s@evil.com\r\nSubject: Win big!\r\n"
	lines := []string{
		fmt.Sprintf(`* 1 FETCH (UID 7 FLAGS () BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}`, len(hdr)),
		hdr, `)`,
	}
	e := parseSingleFetch(lines, "spam", "Spam")
	if e == nil {
		t.Fatal("nil result")
	}
	if e.GmailLabel != "Spam" {
		t.Errorf("GmailLabel: got %q", e.GmailLabel)
	}
}

// ── parseFetchLines ───────────────────────────────────────────────────────────

func TestParseFetchLines_MultipleMessages(t *testing.T) {
	block := func(uid, subj string) []string {
		hdr := fmt.Sprintf("Subject: %s\r\n", subj)
		return []string{
			fmt.Sprintf(`* %s FETCH (UID %s FLAGS () BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}`, uid, uid, len(hdr)),
			hdr, `)`,
		}
	}
	var all []string
	all = append(all, block("1", "Alpha")...)
	all = append(all, block("2", "Beta")...)
	all = append(all, block("3", "Gamma")...)

	seen := map[string]bool{}
	emails := parseFetchLines(all, "all", "All Mail", seen)
	if len(emails) != 3 {
		t.Errorf("want 3, got %d", len(emails))
	}
}

func TestParseFetchLines_SkipsSeenIDs(t *testing.T) {
	hdr := "Subject: Dup\r\n"
	lines := []string{
		fmt.Sprintf(`* 1 FETCH (UID 99 FLAGS () BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}`, len(hdr)),
		hdr, `)`,
	}
	seen := map[string]bool{"imap:all:99": true}
	if emails := parseFetchLines(lines, "all", "All Mail", seen); len(emails) != 0 {
		t.Errorf("want 0 (already seen), got %d", len(emails))
	}
}

// ── matchesAny ───────────────────────────────────────────────────────────────

func TestMatchesAny(t *testing.T) {
	kws := []string{"hello", "world"}
	if !matchesAny("say hello there", kws) {
		t.Error("expected match for 'hello'")
	}
	if matchesAny("nothing relevant", kws) {
		t.Error("expected no match")
	}
}

// ── quoteArg ─────────────────────────────────────────────────────────────────

func TestQuoteArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"user@example.com", `"user@example.com"`},
		{`say "hello"`, `"say \"hello\""`},
		{`C:\path`, `"C:\\path"`},
	}
	for _, tc := range cases {
		if got := quoteArg(tc.in); got != tc.want {
			t.Errorf("quoteArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func makeBenchEmails(n int) []Email {
	subjects := []string{
		"50% off your next order!", "Weekly Digest #42", "You have won a prize!",
		"Your order confirmation #999", "Security alert new login",
		"Hey catching up soon?", "Invoice from Acme Corp",
		"noreply welcome to our platform", "LinkedIn: someone viewed your profile",
	}
	senders := []string{
		"shop@store.com", "digest@news.org", "noreply@scam.com",
		"orders@amazon.com", "security@google.com", "friend@gmail.com",
		"billing@acme.com", "noreply@service.net", "notifications@linkedin.com",
	}
	emails := make([]Email, n)
	for i := range emails {
		j := i % len(subjects)
		emails[i] = Email{
			ID:          fmt.Sprintf("imap:all:%d", i+1),
			Subject:     subjects[j],
			SenderEmail: senders[j],
			Domain:      senders[j][strings.Index(senders[j], "@")+1:],
			Unread:      i%3 == 0,
		}
	}
	return emails
}

func BenchmarkCategorizeOne(b *testing.B) {
	e := Email{Subject: "50% off your next order!", SenderEmail: "shop@store.com", Domain: "store.com"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		categorizeOne(&e)
	}
}

func BenchmarkCategorizeEmails_1k(b *testing.B) {
	emails := makeBenchEmails(1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cp := make([]Email, len(emails))
		copy(cp, emails)
		categorizeEmails(cp)
	}
}

func BenchmarkCategorizeEmails_40k(b *testing.B) {
	emails := makeBenchEmails(40000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cp := make([]Email, len(emails))
		copy(cp, emails)
		categorizeEmails(cp)
	}
}

func BenchmarkRebuildFromCached_40k(b *testing.B) {
	emails := makeBenchEmails(40000)
	for i := range emails {
		emails[i].Category = CatPromotional
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rebuildFromCached(emails)
	}
}

func BenchmarkParseSingleFetch(b *testing.B) {
	hdr := "From: Alice <alice@example.com>\r\nSubject: Test Email\r\nDate: Mon, 01 Jan 2024 12:00:00 +0000\r\n"
	lines := []string{
		fmt.Sprintf(`* 1 FETCH (UID 42 FLAGS (\Seen) BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}`, len(hdr)),
		hdr, `)`,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseSingleFetch(lines, "all", "All Mail")
	}
}
