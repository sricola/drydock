package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"drydock/internal/config"
	"drydock/internal/gateway"
	"drydock/internal/provider"
)

// openAICompatWarnings inspects the openai_compat lane config and returns one
// human-readable warning string per detected misconfiguration. Returns nil
// when the lane is disabled (oc.BaseURL == ""). The function is pure and never
// panics — URL parse errors are silently skipped (config.validate() already
// rejects unparseable URLs).
func openAICompatWarnings(oc config.OpenAICompatConfig) []string {
	if oc.BaseURL == "" {
		return nil
	}
	var warnings []string

	// Check for negative prices — a negative price means USD budget never trips.
	for model, pr := range oc.Prices {
		if pr.Input < 0 || pr.Output < 0 {
			warnings = append(warnings, fmt.Sprintf(
				`openai_compat.prices[%q] has a negative value; a negative price makes the USD budget never trip — spend will be uncapped except by task_max_requests`,
				model,
			))
		}
	}

	// Check for prices map with no "default" entry — unlisted models meter at $0.
	if len(oc.Prices) > 0 {
		if _, hasDefault := oc.Prices["default"]; !hasDefault {
			warnings = append(warnings,
				`openai_compat.prices is set but has no "default" entry; any model not listed is metered at $0 (uncapped USD spend) — add a "default" row or rely on task_max_requests`,
			)
		}
	}

	// Check whether base_url carries a path — that path is ignored by the gateway.
	if u, err := url.Parse(oc.BaseURL); err == nil {
		if p := u.Path; p != "" && p != "/" {
			warnings = append(warnings, fmt.Sprintf(
				`openai_compat.base_url has a path (%q); that path is ignored by the gateway — move it to openai_compat.base_path`,
				p,
			))
		}
	}

	// Check whether base_path is missing its leading slash — causes mis-joins.
	if oc.BasePath != "" && !strings.HasPrefix(oc.BasePath, "/") {
		warnings = append(warnings, fmt.Sprintf(
			`openai_compat.base_path (%q) does not start with "/"; the upstream path will be mis-joined — prefix it with "/"`,
			oc.BasePath,
		))
	}

	return warnings
}

// buildBackends constructs the gateway backends from cfg and fileKeys.
// fileKeys supplements env-var lookup (see resolveAPIKey) without touching
// the process environment — useful for testing.
// Returns an error for any misconfigured or absent credentials.
func buildBackends(cfg *config.Config, fileKeys map[string]string) ([]gateway.Backend, error) {
	var backends []gateway.Backend
	for _, p := range provider.Registry {
		if p.ConfigBuilt {
			oc := cfg.OpenAICompat
			if oc.BaseURL == "" {
				continue // provider not configured; skip
			}
			key := resolveAPIKey(oc.APIKeyEnv, fileKeys)
			if key == "" {
				return nil, fmt.Errorf("openai_compat.base_url is set but its api_key_env (%s) is empty", oc.APIKeyEnv)
			}
			prices := map[string]gateway.Price{}
			for m, pr := range oc.Prices {
				prices[m] = gateway.Price{InputPer1M: pr.Input, OutputPer1M: pr.Output}
			}
			backends = append(backends, gateway.Backend{
				Vendor: gateway.OpenAICompatVendor(p.Vendor, oc.BaseURL, oc.BasePath, prices),
				Cred:   gateway.StaticKey(key),
			})
			continue
		}
		switch cfg.AuthMode(p.Vendor) {
		case "subscription":
			b, err := p.OAuthBackend(config.Dir())
			if err != nil {
				return nil, fmt.Errorf("%s_auth=subscription but no usable credentials — run `%s`: %w", p.Vendor, p.AuthCmd, err)
			}
			backends = append(backends, b)
		default: // api_key
			if key := resolveAPIKey(p.APIKeyEnv, fileKeys); key != "" {
				backends = append(backends, gateway.Backend{Vendor: p.APIVendor(), Cred: gateway.StaticKey(key)})
			}
		}
	}
	if len(backends) == 0 {
		return nil, errors.New("set at least one provider's API key (e.g. ANTHROPIC_API_KEY/OPENAI_API_KEY) or enable a subscription mode")
	}
	return backends, nil
}
