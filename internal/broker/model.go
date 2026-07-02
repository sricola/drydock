package broker

import "drydock/internal/provider"

// taskModelFor picks the per-task model before the operator default is applied.
// An explicit --model always wins. Otherwise, providers that NeedsModel (those
// with no built-in default, like the openai-compat lane) fall back to the
// configured openai_compat.model.
func taskModelFor(taskModel, openAICompatModel, vendor string) string {
	if taskModel == "" {
		if p, ok := provider.ByVendor(vendor); ok && p.NeedsModel {
			return openAICompatModel
		}
	}
	return taskModel
}

// effectiveDefaultModel applies the operator DefaultModel only where it makes
// sense. The operator default is claude/codex-oriented; it must not leak into
// providers with NoOperatorDefault (e.g. opencode — it'd become
// `-m drydock/<claude-model>` and not resolve).
// For those providers the model comes only from --model or openai_compat.model.
func effectiveDefaultModel(operatorDefault, vendor string) string {
	if p, ok := provider.ByVendor(vendor); ok && p.NoOperatorDefault {
		return ""
	}
	return operatorDefault
}

// modelEnv resolves the model passthrough for a task: the per-task value wins,
// then the operator default. When both are empty the env stays unset so
// entrypoint.sh skips `--model` and claude-code picks its own default.
func modelEnv(taskModel, defaultModel string) []string {
	switch {
	case taskModel != "":
		return []string{"DRYDOCK_MODEL=" + taskModel}
	case defaultModel != "":
		return []string{"DRYDOCK_MODEL=" + defaultModel}
	}
	return nil
}
