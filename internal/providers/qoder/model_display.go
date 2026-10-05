package qoder

// Display-catalog decoration: raw native catalog rows gain the console's
// context/pricing keys. These mirror what the worker forwarded.

// ApplyModelContext writes the display catalog's context keys from the context
// metadata Qoder reports per model. The upstream forwards
// `default_context_window` (the window the Qoder client selects by default)
// and `available_context_windows` (the selectable set); the console reads
// `catalog_context_length` / `catalog_context_length_max`. Without this the
// console fell back to a hardcoded window (180000) for every model, mis-sizing
// models such as glm-5.3-flash (1M) and deepseek-v4-pro (96K). Keys already
// present are left as-is, and nothing is written when Qoder reported no
// context metadata.
func ApplyModelContext(entry map[string]any) {
	if entry == nil {
		return
	}
	if _, ok := entry["catalog_context_length"]; !ok {
		if window, ok := numberFieldValue(entry, "default_context_window"); ok && window > 0 {
			entry["catalog_context_length"] = window
		} else if window, ok := numberFieldValue(entry, "context_length"); ok && window > 0 {
			entry["catalog_context_length"] = window
		}
	}
	if _, ok := entry["catalog_context_length_max"]; !ok {
		if maxWindow, ok := largestContextWindow(entry); ok {
			entry["catalog_context_length_max"] = maxWindow
		}
	}
	if _, ok := entry["max_output_tokens"]; !ok {
		if output, ok := numberFieldValue(entry, "max_output_tokens"); ok && output > 0 {
			entry["max_output_tokens"] = output
		}
	}
}

// ApplyModelPricing writes the display catalog's credits/free keys onto a raw
// Qoder model entry. Upstream reports `price_factor` (the multiplier the Qoder
// client renders as e.g. "1.50x") and `is_free`; the console and gateway read
// `credits`/`free`. Qoder reports the price per model, so no cross-region
// reconciliation is needed. Keys already present are left as-is, and nothing
// is written when Qoder reported no price, so an unpriced model is never shown
// as free.
func ApplyModelPricing(entry map[string]any) {
	if entry == nil {
		return
	}
	if _, ok := entry["credits"]; !ok {
		if credits := qoderEntryCredits(entry); credits != "" {
			entry["credits"] = credits
		}
	}
	if _, ok := entry["free"]; !ok {
		if qoderEntryFree(entry) {
			entry["free"] = true
		}
	}
}
