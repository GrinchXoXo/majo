package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPProvider implements Provider by calling the Python AI service over HTTP.
// This is the "InitialLLMProvider" referenced in PRD 0 FINAL §18 — it is not
// coupled to any specific model vendor; that choice lives inside the Python
// service and can change without touching this code.
type HTTPProvider struct {
	baseURL string
	client  *http.Client
}

func NewHTTPProvider(baseURL string) *HTTPProvider {
	return &HTTPProvider{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *HTTPProvider) Extract(ctx context.Context, req ExtractionRequest) (*ExtractionResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling extraction request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/extract", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building extraction request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling AI service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AI service returned status %d", resp.StatusCode)
	}

	var result ExtractionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding extraction response: %w", err)
	}

	return &result, nil
}

func (p *HTTPProvider) Healthy(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
	if err != nil {
		return err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("AI service unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("AI service unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// ExtractSupply calls the Python /extract-supply endpoint (PRD 2 §13-14).
func (p *HTTPProvider) ExtractSupply(ctx context.Context, text string) (*SupplyExtractionResult, error) {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, fmt.Errorf("marshaling supply extraction request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/extract-supply", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building supply extraction request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling AI service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AI service returned status %d", resp.StatusCode)
	}

	var result SupplyExtractionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding supply extraction response: %w", err)
	}
	return &result, nil
}

// ExtractVendorResponse calls the Python /extract-vendor-response endpoint
// (PRD 2 §34-38).
func (p *HTTPProvider) ExtractVendorResponse(ctx context.Context, text string) (*VendorResponseExtractionResult, error) {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, fmt.Errorf("marshaling vendor response extraction request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/extract-vendor-response", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building vendor response extraction request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling AI service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AI service returned status %d", resp.StatusCode)
	}

	var result VendorResponseExtractionResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding vendor response extraction response: %w", err)
	}
	return &result, nil
}
