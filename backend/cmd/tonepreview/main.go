// Command tonepreview shows what the tone layer (pilot batch item 7) would
// send for each kind of buyer message, before TONE_LAYER is switched on.
//
// Start the AI service with your key (ANTHROPIC_API_KEY in its environment),
// then from backend/:
//
//	go run ./cmd/tonepreview                      # AI service on localhost:8000
//	go run ./cmd/tonepreview -ai http://localhost:8000 -only deal_question
//
// For every sample it prints the template text, what the model saw (facts
// masked), and what the buyer would get, or why the original would be sent
// instead. Nothing is sent to WhatsApp and nothing is written anywhere.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/tone"
)

func main() {
	aiURL := flag.String("ai", "http://localhost:8000", "AI service base URL")
	only := flag.String("only", "", "run one sample by name")
	timeout := flag.Duration("timeout", 10*time.Second, "per-message timeout")
	flag.Parse()

	rw := ai.NewHTTPProvider(*aiURL)
	layer := tone.New(tone.Config{Mode: tone.On, Timeout: *timeout}, rw, nil)
	results := map[string]int{}
	for _, s := range tone.Samples {
		if *only != "" && s.Name != *only {
			continue
		}
		m := tone.Mask(s.Text, s.Keep)
		out, meta := layer.Tone(context.Background(), s.Text, s.Keep)
		result, _ := meta["result"].(string)
		if reason, ok := meta["reason"].(string); ok {
			result += " (" + reason + ")"
		}
		if e, ok := meta["error"].(string); ok {
			result += " (" + e + ")"
		}
		results[result]++
		fmt.Printf("=== %s: %s\n--- template:\n%s\n--- model saw:\n%s\n", s.Name, result, s.Text, m.Text)
		if c, ok := meta["candidate"].(string); ok {
			fmt.Printf("--- model's rewrite (NOT sent):\n%s\n", c)
		}
		fmt.Printf("--- buyer gets:\n%s\n\n", out)
	}
	keys := make([]string, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("Summary:")
	for _, k := range keys {
		fmt.Printf("  %3d  %s\n", results[k], k)
	}
	if results["applied"] == 0 {
		os.Exit(1)
	}
}
