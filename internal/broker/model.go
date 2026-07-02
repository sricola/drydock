package broker

// taskModelFor picks the per-task model before the operator default is applied.
// An explicit --model always wins. Otherwise the openai-compat vendor
// (opencode) falls back to the configured openai_compat.model, since that lane
// has no built-in model the way claude/codex do.
func taskModelFor(taskModel, openAICompatModel, vendor string) string {
	if taskModel == "" && vendor == "openai-compat" {
		return openAICompatModel
	}
	return taskModel
}

// effectiveDefaultModel applies the operator DefaultModel only where it makes
// sense. The operator default is claude/codex-oriented; it must not leak into
// the opencode lane (it'd become `-m drydock/<claude-model>` and not resolve).
// For openai-compat the model comes only from --model or openai_compat.model.
func effectiveDefaultModel(operatorDefault, vendor string) string {
	if vendor == "openai-compat" {
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
