package remote

// PushOnlyAdapter is the no-op for generic git URLs (self-hosted servers
// with no PR/MR concept on drydock's side). stage.Push has already
// pushed the branch; there's nothing else to do, and that's fine —
// the task response still includes the branch name.
type PushOnlyAdapter struct{}

func (PushOnlyAdapter) Name() string { return "push-only" }

func (PushOnlyAdapter) OpenRequest(r Request) error { return nil }

func (PushOnlyAdapter) Available() error { return nil }
