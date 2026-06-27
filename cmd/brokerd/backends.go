package main

import (
	"fmt"

	"drydock/internal/config"
	"drydock/internal/gateway"
	"drydock/internal/provider"
)

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
		return nil, fmt.Errorf("set at least one provider's API key (e.g. ANTHROPIC_API_KEY/OPENAI_API_KEY) or enable a subscription mode")
	}
	return backends, nil
}
