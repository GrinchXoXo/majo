// Package tone is the tone layer (pilot batch item 7): outgoing buyer
// messages are rewritten by an LLM in a natural Nigerian WhatsApp register,
// without changing a single fact.
//
// How facts are kept:
//
//  1. Mask: every fact in the template text is replaced by a token [[F1]],
//     [[F2]], ...: whole list/label/link lines, numbers and amounts, links,
//     references, reply keywords (YES, NOT YET), quoted text, names
//     (capitalised words), and the terms the caller marks (product, seller).
//     The model never sees the facts, so prices and phone numbers do not
//     reach it.
//  2. Rewrite: the AI service rewrites the masked text.
//  3. Check: the rewrite must keep every token once, in order; add no digit,
//     link, number word, time word or claim (free, delivery, discount...);
//     keep every uncommon word (item words); keep negation and questions;
//     stay about the same length.
//  4. Unmask, or, if any check fails or the call fails or times out, send
//     the original template text. A fact can therefore never change.
//
// Modes: off (default: no AI call), shadow (rewrite and check, store the
// result, send the original), on (send the rewrite when it passes).
package tone

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Mode of the tone layer.
type Mode string

const (
	Off    Mode = "off"
	Shadow Mode = "shadow"
	On     Mode = "on"
)

// ParseMode reads TONE_LAYER ("" = off).
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "false":
		return Off, nil
	case "shadow":
		return Shadow, nil
	case "on", "true":
		return On, nil
	}
	return Off, fmt.Errorf("TONE_LAYER must be off, shadow or on, got %q", s)
}

// Rewriter is the AI service (POST /tone/rewrite). It receives masked text only.
type Rewriter interface {
	RewriteTone(ctx context.Context, masked string) (text, model string, err error)
}

// Config of the layer.
type Config struct {
	Mode    Mode
	Timeout time.Duration // per rewrite; default 5s
	MaxLen  int           // longer messages are sent as they are; default 1200 bytes
}

// Layer implements conversation.Toner.
type Layer struct {
	cfg    Config
	rw     Rewriter
	logger *slog.Logger
}

// New builds the layer. A nil Rewriter means off.
func New(cfg Config, rw Rewriter, logger *slog.Logger) *Layer {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.MaxLen <= 0 {
		cfg.MaxLen = 1200
	}
	if rw == nil {
		cfg.Mode = Off
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Layer{cfg: cfg, rw: rw, logger: logger}
}

// Mode reports the configured mode.
func (l *Layer) Mode() Mode { return l.cfg.Mode }

// Tone returns the text to send and metadata to store with the message
// (nil when the layer did nothing). keep lists exact phrases that must not
// change (item, seller name); they are masked like any other fact.
func (l *Layer) Tone(ctx context.Context, text string, keep []string) (string, map[string]any) {
	if l == nil || l.cfg.Mode == Off || strings.TrimSpace(text) == "" {
		return text, nil
	}
	meta := map[string]any{"mode": string(l.cfg.Mode)}
	if len(text) > l.cfg.MaxLen {
		meta["result"] = "skipped_too_long"
		return text, meta
	}
	if strings.Contains(text, "[[F") || !utf8.ValidString(text) {
		meta["result"] = "skipped_token_like_text"
		return text, meta
	}
	m := Mask(text, keep)
	if !m.HasProse() {
		meta["result"] = "skipped_no_prose"
		return text, meta
	}
	rctx, cancel := context.WithTimeout(ctx, l.cfg.Timeout)
	defer cancel()
	start := time.Now()
	rewritten, model, err := l.rw.RewriteTone(rctx, m.Text)
	meta["ms"] = time.Since(start).Milliseconds()
	if model != "" {
		meta["model"] = model
	}
	if err != nil {
		meta["result"] = "rewrite_failed"
		meta["error"] = err.Error()
		l.logger.Info("tone: rewrite unavailable, sending the original", "error", err)
		return text, meta
	}
	if reason := Check(m, rewritten); reason != "" {
		meta["result"] = "rejected"
		meta["reason"] = reason
		meta["candidate"] = m.Unmask(rewritten)
		l.logger.Info("tone: rewrite rejected, sending the original", "reason", reason)
		return text, meta
	}
	final := m.Unmask(rewritten)
	if l.cfg.Mode == Shadow {
		meta["result"] = "shadow"
		meta["candidate"] = final
		return text, meta
	}
	meta["result"] = "applied"
	meta["original"] = text
	return final, meta
}

// --- masking ---------------------------------------------------------------

// Masked is a text with its facts replaced by tokens.
type Masked struct {
	Original string
	Text     string   // with [[F1]].. tokens
	Facts    []string // Facts[i] is the value of [[F{i+1}]]
}

func token(i int) string { return "[[F" + strconv.Itoa(i) + "]]" }

var tokenRe = regexp.MustCompile(`\[\[F(\d+)\]\]`)

// HasProse: is there anything left to rewrite once the facts are masked?
func (m Masked) HasProse() bool {
	rest := tokenRe.ReplaceAllString(m.Text, "")
	return len(wordsOf(rest)) >= 2
}

// Unmask puts the facts back.
func (m Masked) Unmask(s string) string {
	return tokenRe.ReplaceAllStringFunc(s, func(t string) string {
		n, _ := strconv.Atoi(tokenRe.FindStringSubmatch(t)[1])
		if n >= 1 && n <= len(m.Facts) {
			return m.Facts[n-1]
		}
		return t
	})
}

var (
	// Whole lines that are data, not prose: list items, "Label: value"
	// lines, and lines with a link.
	listLine  = regexp.MustCompile(`^\s*(\d+[.)]|[-•*▪>]|[\p{So}]\s*\d)\s*`)
	labelLine = regexp.MustCompile(`^\s*[\p{L}][\p{L} '’]{0,24}:\s*`)
	linkRe    = regexp.MustCompile(`(?i)(https?://\S+|www\.\S+|wa\.me/\S+|\S+@\S+\.\S+)`)

	// Inline facts, in priority order.
	refRe     = regexp.MustCompile(`\b[A-Z]{2,}-[A-Z0-9]*\d[A-Z0-9-]*\b`)
	quotedRe  = regexp.MustCompile(`"[^"\n]{1,60}"|“[^”\n]{1,60}”`)
	rangeRe   = regexp.MustCompile(`(?i)(?:₦\s?)?\d[\d,.]*\s?k?\s?[-–]\s?(?:₦\s?)?\d[\d,.]*\s?k?\b`)
	amountRe  = regexp.MustCompile(`(?i)(?:₦\s?)?\d[\d,.:/]*(?:\s?(?:k|m|million|thousand|naira|gb|tb|mah|kg|g|ml|l|cl|inch|inches|hrs?|mins?|days?|weeks?|pcs|am|pm)\b|%)?|\bN\d[\d,.]*`)
	keywordRe = regexp.MustCompile(`\b[A-Z]{2,}(?:[ ]+[A-Z]{2,})*\b`)
	nameRe    = regexp.MustCompile(`[\p{Lu}][\p{L}\p{N}'’&\-]*(?:[ ]+(?:&[ ]+)?[\p{Lu}\p{N}][\p{L}\p{N}'’&\-]*)*`)
)

type span struct{ start, end int }

// Mask replaces the facts in text by tokens.
func Mask(text string, keep []string) Masked {
	m := Masked{Original: text}
	var out strings.Builder
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			out.WriteString(line)
			continue
		}
		lead := line[:len(line)-len(strings.TrimLeftFunc(line, unicode.IsSpace))]
		if loc := labelLine.FindStringIndex(line); loc != nil && !dataLine(line) {
			// "Earlier update: <sentence>": the label is a fact, the rest is prose.
			label := line[len(lead):loc[1]]
			core := strings.TrimRight(label, " \t")
			m.Facts = append(m.Facts, core)
			out.WriteString(lead + token(len(m.Facts)) + label[len(core):] + m.maskLine(line[loc[1]:], keep))
			continue
		}
		if dataLine(line) {
			m.Facts = append(m.Facts, trimmed)
			out.WriteString(lead + token(len(m.Facts)) + line[len(lead)+len(trimmed):])
			continue
		}
		out.WriteString(m.maskLine(line, keep))
	}
	m.Text = out.String()
	return m
}

// shortLabelLine: "Seller: Mike Gadgets", not "Note: <a sentence or two>".
func shortLabelLine(line string) bool {
	loc := labelLine.FindStringIndex(line)
	if loc == nil {
		return false
	}
	v := strings.TrimSpace(line[loc[1]:])
	return v != "" && utf8.RuneCountInString(v) <= 60 && !strings.Contains(v, ". ") && !strings.Contains(v, "? ")
}

// dataLine: a line that is data, not prose. It is kept verbatim: list
// items and their indented detail lines, "Label: value" lines, lines with a
// link, and "a · b" detail lines.
func dataLine(line string) bool {
	return listLine.MatchString(line) || shortLabelLine(line) || linkRe.MatchString(line) ||
		strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") || strings.Contains(line, " · ")
}

func (m *Masked) maskLine(line string, keep []string) string {
	var spans []span
	taken := func(s, e int) bool {
		for _, sp := range spans {
			if s < sp.end && e > sp.start {
				return true
			}
		}
		return false
	}
	add := func(s, e int) {
		// trim spaces/punctuation the patterns may have caught at the edges
		for s < e && strings.ContainsRune(" \t", rune(line[s])) {
			s++
		}
		for e > s && strings.ContainsRune(" \t.,;!?", rune(line[e-1])) && !strings.HasSuffix(line[s:e], "..") {
			e--
		}
		if s < e && !taken(s, e) {
			spans = append(spans, span{s, e})
		}
	}

	// 1. caller's terms, longest first, case-insensitive
	terms := append([]string(nil), keep...)
	sort.Slice(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if len([]rune(t)) < 2 {
			continue
		}
		// Matched on the line itself: lower-casing can change byte lengths.
		re, err := regexp.Compile(`(?i)` + regexp.QuoteMeta(t))
		if err != nil {
			continue // not valid UTF-8
		}
		for _, loc := range re.FindAllStringIndex(line, -1) {
			if boundary(line, loc[0], loc[1]) {
				add(loc[0], loc[1])
			}
		}
	}
	// 2. links, references, quoted text, amounts and numbers, keywords
	for _, re := range []*regexp.Regexp{linkRe, refRe, quotedRe, rangeRe, amountRe, keywordRe} {
		for _, loc := range re.FindAllStringIndex(line, -1) {
			add(loc[0], loc[1])
		}
	}
	// 3. names: capitalised words, except a single ordinary word starting a sentence
	for _, loc := range nameRe.FindAllStringIndex(line, -1) {
		s, e := loc[0], loc[1]
		start := sentenceStart(line, s)
		// "Can I", "Thanks Ada": an ordinary first word of a sentence is not
		// part of the name; "I" is never part of one.
		for {
			w := line[s:e]
			if i := strings.LastIndexByte(w, ' '); i > 0 && isI(w[i+1:]) {
				e = s + strings.LastIndexFunc(w[:i], func(r rune) bool { return r != ' ' }) + 1
				continue
			}
			if i := strings.IndexByte(w, ' '); i > 0 && start && common[strings.ToLower(w[:i])] {
				s += i + strings.IndexFunc(w[i:], func(r rune) bool { return r != ' ' })
				start = false
				continue
			}
			break
		}
		w := line[s:e]
		if isI(w) {
			continue
		}
		if start && !strings.Contains(w, " ") && common[strings.ToLower(strings.Trim(w, "'’"))] {
			continue
		}
		if start && !strings.Contains(w, " ") && !hasInnerCap(w) {
			// "Thanks", "Okay", "Sorry": a sentence opener, not a name. A name
			// at the start of a sentence is masked through keep terms.
			continue
		}
		add(s, e)
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(line[last:sp.start])
		m.Facts = append(m.Facts, line[sp.start:sp.end])
		b.WriteString(token(len(m.Facts)))
		last = sp.end
	}
	b.WriteString(line[last:])
	return b.String()
}

func boundary(s string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	if end < len(s) {
		r, _ := utf8.DecodeRuneInString(s[end:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func isI(w string) bool {
	return w == "I" || strings.HasPrefix(w, "I'") || strings.HasPrefix(w, "I’")
}

func hasInnerCap(w string) bool {
	for i, r := range w {
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// sentenceStart: only spaces, emoji or opening punctuation since the start
// of the line or the last sentence end.
func sentenceStart(line string, at int) bool {
	for i := at; i > 0; {
		r, size := utf8.DecodeLastRuneInString(line[:i])
		i -= size
		switch {
		case r == '.' || r == '!' || r == '?' || r == ':':
			return true
		case unicode.IsSpace(r) || unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) || r == '(' || r == '"' || r == '“' || r == '‍' || r == '️':
			continue
		default:
			return false
		}
	}
	return true
}

// --- checks ----------------------------------------------------------------

// Check returns why a rewrite of m cannot be sent, or "" if it can.
//
// The rewrite may only (a) add words from a small register list (Pidgin
// particles, greetings, auxiliaries: "abeg", "o", "don", "dey"...), (b)
// drop filler words ("the", "please", "did", "will"...), and (c) map a few
// Pidgin equivalents ("wetin" = what, "fit" = can, "no" = not). Every other
// word of the template, and every fact token, must still be there in the
// same order. So a rewrite cannot swap "bad" and "great", "before" and
// "after", YES and NO, add "free" or "without", or drop an item word
// (independent review of pilot batch 7).
func Check(m Masked, rewritten string) string {
	r := strings.TrimSpace(rewritten)
	if r == "" {
		return "empty"
	}
	if strings.Join(tokenRe.FindAllString(m.Text, -1), " ") != strings.Join(tokenRe.FindAllString(r, -1), " ") {
		return "tokens_changed"
	}
	origRest := tokenRe.ReplaceAllString(m.Text, " ")
	rest := tokenRe.ReplaceAllString(r, " ")
	if strings.Contains(rest, "[[") || strings.Contains(rest, "]]") {
		return "tokens_changed"
	}
	if strings.ContainsRune(rest, '₦') || strings.IndexFunc(rest, unicode.IsDigit) >= 0 {
		return "digits_added"
	}
	if linkRe.MatchString(rest) || bareDomain.MatchString(rest) || strings.Contains(rest, "@") {
		return "link_added"
	}
	ow, rw := canonWords(m.Text), canonWords(r)
	inOrig := map[string]bool{}
	for _, w := range ow {
		inOrig[w] = true
	}
	for _, w := range rw {
		if !inOrig[w] && !tokenRe.MatchString(w) && !register[w] {
			return "added:" + w
		}
	}
	// The template's meaningful words and tokens, in order, must be a
	// subsequence of the rewrite's.
	i := 0
	for _, w := range ow {
		if droppable[w] {
			continue
		}
		for i < len(rw) && rw[i] != w {
			i++
		}
		if i == len(rw) {
			return "changed:" + w
		}
		i++
	}
	if negated(origRest) != negated(rest) {
		return "negation_changed"
	}
	if strings.Contains(origRest, "?") && !strings.Contains(rest, "?") {
		return "question_lost"
	}
	n, k := utf8.RuneCountInString(m.Text), utf8.RuneCountInString(r)
	if k > 2*n+40 || 3*k < n {
		return "length"
	}
	return ""
}

// A bare domain ("bit.ly/x", "majo.ng") is a link too.
var bareDomain = regexp.MustCompile(`(?i)\b[\p{L}\p{N}-]+\.(com|ng|ly|io|co|me|net|org|app|link|xyz|info|biz)\b`)

var (
	canonTokenRe = regexp.MustCompile(`\[\[F\d+\]\]|[\p{L}]+(?:'[\p{L}]+)*`)
	// Fixed expressions that carry no fact; removed before comparing.
	fixedPhrases = regexp.MustCompile(`(?i)\b(no wahala|no problem|no worries|no shaking|how far)\b`)
	contractions = []struct{ suffix, expand string }{
		{"n't", " not"}, {"'ll", " will"}, {"'m", " am"}, {"'re", " are"}, {"'ve", " have"}, {"'d", " would"}, {"'s", ""},
	}
	irregularNot = map[string]string{"won't": "will not", "can't": "can not", "cannot": "can not", "shan't": "shall not", "ain't": "not"}
	synonyms     = map[string]string{"could": "can", "fit": "can", "wetin": "what", "una": "you", "no": "not", "nor": "not", "never": "not"}
)

// canonWords: the comparison form of a text: tokens kept as they are,
// words lower-cased, contractions expanded, plain plural/tense endings
// removed, Pidgin equivalents mapped, fixed expressions dropped.
func canonWords(s string) []string {
	s = strings.ReplaceAll(strings.ToLower(s), "’", "'")
	s = fixedPhrases.ReplaceAllString(s, " ")
	var out []string
	for _, w := range canonTokenRe.FindAllString(s, -1) {
		if strings.HasPrefix(w, "[[") {
			out = append(out, strings.ToUpper(w))
			continue
		}
		if x, ok := irregularNot[w]; ok {
			w = x
		} else {
			for _, c := range contractions {
				if strings.HasSuffix(w, c.suffix) && len(w) > len(c.suffix) {
					w = strings.TrimSuffix(w, c.suffix) + c.expand
					break
				}
			}
		}
		for _, part := range strings.Fields(w) {
			part = stem(part)
			if syn, ok := synonyms[part]; ok {
				part = syn
			}
			out = append(out, part)
		}
	}
	// Pidgin puts the negation before the modal ("I no fit find" = "I
	// can not find"): compare in the English order.
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "not" && modals[out[i+1]] {
			out[i], out[i+1] = out[i+1], out[i]
			i++
		}
	}
	return out
}

var modals = set("can will do would should shall did doe")

func stem(w string) string {
	switch {
	case len(w) > 4 && (strings.HasSuffix(w, "ies") || strings.HasSuffix(w, "ied")):
		return w[:len(w)-3] + "y"
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:len(w)-1]
	}
	return w
}

// droppable: template words a rewrite may leave out.
var droppable = set(`
a an the hi hello hey thanks thank please kindly just so very really also okay ok alright well great sure good nice
did do doe is are am was were be been being has have had will would shall should it that this there here then too still yet and
`)

// register: words a rewrite may add (Nigerian WhatsApp register and
// grammatical glue that carries no fact).
var register = set(`
o oh abeg pls biko dey don go wey na make sef sha abi e am boss
hi hello hey please kindly thanks thank sorry okay ok alright great nice good
the a an just so very really well and is are be was will have ha do did it
`)

var wordRe = regexp.MustCompile(`[\p{L}]+(?:['’][\p{L}]+)*`)

func wordsOf(s string) []string { return wordRe.FindAllString(s, -1) }

// normWords: lower-case words in order, with ’ read as '.
func normWords(s string) []string {
	ws := wordsOf(strings.ToLower(s))
	for i, w := range ws {
		ws[i] = strings.ReplaceAll(w, "’", "'")
	}
	return ws
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range normWords(s) {
		out[w] = true
	}
	return out
}

// Fixed expressions that contain "no" without negating anything.
var notNegation = regexp.MustCompile(`(?i)\bno\s+(wahala|problem|worries|shaking|vex)\b`)

var negationWords = map[string]bool{
	"no": true, "not": true, "never": true, "nothing": true, "none": true, "nobody": true, "cannot": true,
	"don't": true, "doesn't": true, "didn't": true, "can't": true, "couldn't": true, "won't": true, "wouldn't": true,
	"isn't": true, "aren't": true, "wasn't": true, "weren't": true, "haven't": true, "hasn't": true, "hadn't": true,
	"shouldn't": true, "nor": true,
}

func negated(s string) bool {
	s = notNegation.ReplaceAllString(s, " ")
	for w := range wordSet(s) {
		if negationWords[w] {
			return true
		}
	}
	return false
}

func set(words string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(words) {
		out[w] = true
	}
	return out
}

// risky words may not appear in a rewrite unless the original has them:
// numbers, times, money and promises are facts.
var risky = set(`
two three four five six seven eight nine ten eleven twelve fifteen twenty thirty forty fifty hundred thousand million billion
half double twice first second third
today tomorrow tonight yesterday morning afternoon evening night week weekend month year hour hours minute minutes day days
monday tuesday wednesday thursday friday saturday sunday
soon asap quickly quick fast immediately sharp now later
free discount discounted cheap cheaper cheapest affordable expensive guarantee guaranteed refund warranty original genuine fake legit
verified trusted safe safely certain promise
deliver delivery delivered delivering pickup pay paid payment payments transfer deposit money cash naira price prices cost amount budget
`)

// common words may be dropped or changed freely. Every other word in the
// original (product words, places, specifics) must survive the rewrite.
var common = set(`
a an the and or but so if then than also too very really just only even still already yet again more most much many some any all both each every
other another such same own few lot lots bit little
i me my mine we us our ours you your yours he him his she her it its they them their theirs this that these those there here
i'm i'll i've i'd you're you'll you've you'd we're we'll we've it's that's there's what's who's let's
am is are was were be been being have has had do does did done having doing
will would can could shall should may might must need needs needed want wants wanted
to of in on at by for with from about into onto over under up down out off through between after before during without within
what which who whom whose when where why how
yes no not okay ok alright fine sure please kindly thanks thank thanking welcome sorry hello hi hey dear
good great nice glad happy well right just
get got gets getting go going gone went come coming came give given gave take took make made making keep kept let lets
see seen saw look looking looked find found finding check checking checked
tell told telling say said saying ask asked asking answer answered answers reply replied replies replying respond responded
send sent sending share shared message messages text write writing
know knew known think thought mean means meant like liked
help helping hear heard wait waiting waited
one ones thing things something anything nothing everything someone anyone everyone
back yet till until while whenever whether either neither
search searching searched request requests requested option options choice choices choose chose chosen pick
seller sellers vendor vendors buyer buyers shop shops person people
number numbers detail details info information update updates note noted
area location place
earlier latest last next new
able unable sorry afraid
`)
