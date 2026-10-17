// Package answer reads the short answers to Majo's deal follow-up questions
// (pilot batch item 5). It is a leaf package so both the buyer runtime
// (workflow) and the vendor runtime (routing) can use it: deterministic
// phrase lists, never the AI.
package answer

import (
	"regexp"
	"strings"
)

// Answers.
const (
	Yes    = "YES"
	No     = "NO"
	NotYet = "NOT_YET"
)

// Question kinds (stored on clarifications).
const (
	KindBuyerConfirm  = "deal_confirm"        // "Did you buy ...?"
	KindBuyerRating   = "deal_rating"         // "How would you rate ...? 1-5"
	KindVendorConfirm = "vendor_deal_confirm" // "Did you sell ...?"
)

// IsDealKind: the question is a deal follow-up (asked after the sale; not a
// request-understanding question).
func IsDealKind(kind string) bool {
	return kind == KindBuyerConfirm || kind == KindBuyerRating || kind == KindVendorConfirm
}

// ---- reading answers (deterministic; the AI is never asked) ------------------------------

var (
	yesPhrases = []string{"yes", "yeah", "yea", "yep", "yup", "y", "ya", "sure", "i did", "yes i did", "i bought it", "i bought am",
		"i don buy", "i don buy am", "i buy am", "i don collect", "i don collect am", "e don do", "done", "yes o", "yes oo", "sold",
		"i sold it", "i don sell", "i don sell am", "i sell am", "yes sold", "delivered", "i don deliver", "collected", "we don do am",
		"yes we did", "correct", "ehen", "yes sir", "yes ma", "affirmative", "true"}
	noPhrases = []string{"no", "nope", "nah", "no o", "no oo", "i didn't", "i did not", "i no buy", "i no buy am", "no i didn't",
		"didn't buy", "did not buy", "no sale", "i no sell", "i no sell am", "not sold", "didnt", "no deal", "cancelled", "canceled",
		"e no work", "e no happen", "we no do am", "no we didn't", "never", "no sir", "no ma", "false"}
	notYetPhrases = []string{"not yet", "never yet", "e never reach", "e never happen", "not yet o", "soon", "later", "tomorrow",
		"still waiting", "waiting", "on the way", "e dey come", "e never land", "never land", "pending", "still talking", "we still dey talk"}
	answerSet = buildAnswerSet()
	ratingPat = regexp.MustCompile(`^\s*(?:i\s+(?:give|rate)\s+(?:am|it|them|him|her)?\s*)?([1-5])\s*(?:/\s*5|stars?|⭐+)?\s*[.!]?\s*$`)
	starsPat  = regexp.MustCompile(`^\s*(⭐{1,5})\s*$`)
)

func buildAnswerSet() map[string]string {
	m := map[string]string{}
	for _, p := range yesPhrases {
		m[p] = Yes
	}
	for _, p := range noPhrases {
		m[p] = No
	}
	for _, p := range notYetPhrases {
		m[p] = NotYet
	}
	return m
}

func normalize(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.NewReplacer("’", "'", "👍", " ", "🙏", " ", "✅", " ", "👌", " ", "😊", " ", "🙂", " ").Replace(t)
	t = strings.Trim(t, ".,!? ")
	return strings.Join(strings.Fields(t), " ")
}

// Parse reads a short yes / no / not-yet answer. "" = not an answer
// (Majo asks again, or the message takes the ordinary path).
func Parse(text string) string {
	t := normalize(text)
	if a, ok := answerSet[t]; ok {
		return a
	}
	// "yes, I bought it", "no o, e no work": the first clause decides when
	// it is a clear answer and the message is short.
	if len(strings.Fields(t)) <= 8 {
		for _, sep := range []string{",", ".", "!", " - ", " but "} {
			if i := strings.Index(t, sep); i > 0 {
				if a, ok := answerSet[strings.TrimSpace(t[:i])]; ok {
					return a
				}
			}
		}
	}
	return ""
}

// ParseRating reads a 1-5 rating ("4", "5 stars", "⭐⭐⭐", "i give am 5"). 0 = not a rating.
func ParseRating(text string) int {
	t := strings.ToLower(strings.TrimSpace(text))
	if m := starsPat.FindStringSubmatch(t); m != nil {
		return strings.Count(m[1], "⭐")
	}
	if m := ratingPat.FindStringSubmatch(t); m != nil {
		return int(m[1][0] - '0')
	}
	return 0
}
