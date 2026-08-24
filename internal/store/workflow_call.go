package store

import "sync"

// WorkflowCallBinding links the jobs produced by one reusable-workflow
// call. The gate node resolves the caller's `with:` templates once its
// dependencies finish; called jobs read the resolved inputs and secret
// configuration; the collector maps the called workflow's outputs onto
// the public caller job key.
type WorkflowCallBinding struct {
	// CalledPath identifies the called workflow (for errors/logging).
	CalledPath string
	CalledRepo string
	// With holds the caller's raw input templates; InputDefs the called
	// workflow's declarations (typing + defaults).
	With      map[string]string
	InputDefs map[string]*WorkflowInputDef
	// SecretsInherit / SecretsMap mirror the caller job's `secrets:`.
	SecretsMap     map[string]string
	SecretsInherit bool
	// Parent is the binding of the call this one is nested inside, or nil
	// for a call made by the run's own workflow. `secrets: inherit` inherits
	// the CALLING workflow's secrets, which for a nested call is the parent
	// binding's already-narrowed set rather than the repository's full set,
	// so secret resolution walks this chain from the outermost call inwards.
	Parent *WorkflowCallBinding `json:"-"`
	// OutputDefs holds the called workflow's output value templates.
	OutputDefs map[string]string
	// CalledJobKeys lists the expanded keys of the called workflow's
	// jobs (collector aggregation + output evaluation scope); CallerKey
	// is the public caller job key (the needs-context prefix).
	CalledJobKeys []string
	CallerKey     string

	// ResolvedInputs is filled by the gate node when it completes:
	// the typed `inputs` context the called jobs run with.
	Mu             sync.Mutex `json:"-"`
	resolvedInputs map[string]interface{}
}

// SetResolvedInputs stores the typed inputs resolved at gate completion.
func (b *WorkflowCallBinding) SetResolvedInputs(in map[string]interface{}) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	b.resolvedInputs = in
}

// ResolvedInputs returns the typed inputs resolved at gate completion
// (nil until the gate ran).
func (b *WorkflowCallBinding) ResolvedInputs() map[string]interface{} {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	return b.resolvedInputs
}
