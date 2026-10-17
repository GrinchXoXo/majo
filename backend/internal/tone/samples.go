package tone

// Sample is a buyer message as Majo's templates produce it, with the terms
// the sender marks as fixed. Used by the tests and by cmd/tonepreview.
type Sample struct {
	Name string
	Text string
	Keep []string
}

// Samples covers every kind of buyer message the tone layer may see.
var Samples = []Sample{
	{"greeting", "Hi 👋 I'm Majo. Tell me what you're looking for (and your area) and I'll find it for you.", nil},
	{"ack", "Got it — I'll look for a gas cylinder in Yaba 👀", []string{"gas cylinder"}},
	{"already_on_it", "I'm already on it for iphone 13 👀 I'll send you options as soon as sellers reply.", []string{"iphone 13"}},
	{"ask_model", "Which laptop model (or specs) are you looking for?", nil},
	{"ask_condition", `Do you want it brand new or fairly used? (Reply "new" or "used")`, nil},
	{"ask_budget", "What's your budget for the office chair? A rough range is fine, like 50k-80k.", []string{"office chair"}},
	{"which_first", "I can look for rice, beans and garri. Which should I start with? (I'll take the next one after)", []string{"rice", "beans", "garri"}},
	{"confirm_option", "Do you want option 2 (Mike Gadgets, ₦620,000)? Reply 2 or YES to choose it.", nil},
	{"no_price", "I don't have a price for that yet — I'll send options with prices as soon as sellers reply 👀", nil},
	{"offers", "I found 2 options for iphone 13 👀\n\n1. Mike Gadgets — iphone 13 128GB\n   ₦620,000\n   In stock · used\n\n" +
		"2. Tunde Phones — iphone 13\n   Price on request\n   Can source\n\nReply with the number (1-2) of the one you want.", []string{"iphone 13"}},
	{"vendor_timeout", "The 3 sellers I asked haven't replied yet, so I'm checking with 2 other sellers now 👀", nil},
	{"no_result", "I haven't got a confirmed option for the rechargeable fan yet 😔 The sellers I contacted haven't replied with one. I'll pass on any reply that still comes in.", []string{"rechargeable fan"}},
	{"broaden", "I couldn't find a seller in Yaba. Can I search in nearby areas too? Reply YES or NO.", nil},
	{"late", "Earlier update: I couldn't find a seller in Yaba. Can I search in nearby areas too? Reply YES or NO.", nil},
	{"introduction", "Great choice! 🎉\n\nSeller: Mike Gadgets\nChat with them: https://wa.me/2348000000001\nReference: MM-123456\n\n" +
		"Tip: pay on delivery or after you see the item. Majo doesn't hold or handle payments.", []string{"Mike Gadgets"}},
	{"deal_question", "Hi 👋 did you buy the small chops from Mama Chops? Reply YES, NO or NOT YET.", []string{"small chops", "Mama Chops"}},
	{"rating", "Great! How would you rate Mama Chops from 1 (bad) to 5 (great)? Reply with a number.", []string{"Mama Chops"}},
	{"deal_no", "Thanks for letting me know 🙏 If you still need small chops, just tell me and I'll search again.", []string{"small chops"}},
	{"deal_not_yet", "No wahala 👍 I'll check back later.", nil},
	{"unsupported_media", "I can only read text messages for now 🙏 Please type what you're looking for.", nil},
	{"selection_failed", "Sorry, I couldn't record that choice right now. Please try again in a moment.", nil},
}
