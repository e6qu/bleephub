package store

import "sync"

// WorkflowCallBinding links the jobs produced by one reusable-workflow call:
// the gate resolves the caller's `with:` templates, called jobs read the
// resolved inputs, and the collector maps outputs onto the caller job key.
type WorkflowCallBinding struct {
	CalledPath string
	CalledRepo string
	// With holds the caller's raw input templates; InputDefs the called
	// workflow's declarations (typing + defaults).
	With      map[string]string
	InputDefs map[string]*WorkflowInputDef
	// SecretsInherit / SecretsMap mirror the caller job's `secrets:`.
	SecretsMap     map[string]string
	SecretsInherit bool
	// Parent is the enclosing call's binding, or nil at the top. `secrets:
	// inherit` inherits the CALLING workflow's already-narrowed set, so secret
	// resolution walks this chain outermost-first.
	Parent     *WorkflowCallBinding `json:"-"`
	OutputDefs map[string]string
	// CalledJobKeys are the expanded keys of the called workflow's jobs;
	// CallerKey is the public caller job key (the needs-context prefix).
	CalledJobKeys []string
	CallerKey     string

	Mu             sync.Mutex `json:"-"`
	resolvedInputs map[string]interface{}
}

func (b *WorkflowCallBinding) SetResolvedInputs(in map[string]interface{}) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	b.resolvedInputs = in
}

// ResolvedInputs returns the typed inputs, nil until the gate ran.
func (b *WorkflowCallBinding) ResolvedInputs() map[string]interface{} {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	return b.resolvedInputs
}
