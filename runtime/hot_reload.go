package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// RevisionInfo identifies the complete program graph currently bound to an Instance.
type RevisionInfo struct {
	Generation  uint64
	Hash        string
	SymbolsHash string
}

// PatchPlan is an immutable, validated transition between two complete programs.
type PatchPlan struct {
	base           *Program
	mu             sync.Mutex
	owner          *Instance
	baseGeneration uint64
	baseHash       string
	target         *Program
	changedModules []string
	state          patchPlanState
}

type patchPlanState uint8

const (
	patchPlanPreparing patchPlanState = iota
	patchPlanReady
	patchPlanApplying
	patchPlanCommitted
	patchPlanClosed
)

// PatchResult describes an atomically committed program revision.
type PatchResult struct {
	Previous       RevisionInfo
	Current        RevisionInfo
	ChangedModules []string
}

// Close abandons a prepared patch.
func (p *PatchPlan) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.state == patchPlanClosed || p.state == patchPlanCommitted {
		p.mu.Unlock()
		return nil
	}
	if p.state == patchPlanApplying {
		p.mu.Unlock()
		return errors.New("patch application is in progress")
	}
	p.state = patchPlanClosed
	owner := p.owner
	p.owner = nil
	p.target = nil
	p.base = nil
	p.changedModules = nil
	p.mu.Unlock()
	if owner != nil {
		owner.patchMu.Lock()
		if owner.pendingPatch == p {
			owner.pendingPatch = nil
		}
		owner.patchMu.Unlock()
	}
	return nil
}

// PatchError reports a stable hot-update failure category.
type PatchError struct {
	Code    string
	Message string
}

func (e PatchError) Error() string {
	if e.Message == "" {
		return "hot update failed: " + e.Code
	}
	return "hot update failed: " + e.Code + ": " + e.Message
}

// PreparePatch validates a complete target revision without changing the active revision.
func (i *Instance) PreparePatch(ctx context.Context, next *Program) (*PatchPlan, error) {
	if i == nil || i.vm == nil {
		return nil, PatchError{Code: "closed", Message: "instance is closed"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := i.vm.enterOwnerContext(ctx); err != nil {
		return nil, err
	}
	if !i.isOpen() {
		i.vm.leaveOwner()
		if i.lifecycleState() == instanceFaulted {
			return nil, PatchError{Code: "instance_faulted", Message: ErrInstanceFaulted.Error()}
		}
		return nil, PatchError{Code: "closed", Message: "instance is closed"}
	}
	current := i.vm.revision.Load()
	if current == nil || current.code == nil {
		i.vm.leaveOwner()
		return nil, PatchError{Code: "closed", Message: "instance has no active revision"}
	}
	i.patchMu.Lock()
	pending := i.pendingPatch != nil
	i.patchMu.Unlock()
	if pending {
		i.vm.leaveOwner()
		return nil, PatchError{Code: "patch_pending", Message: "another patch plan is pending"}
	}
	if next == nil || next.code == nil {
		i.vm.leaveOwner()
		return nil, PatchError{Code: "invalid_program", Message: "target program is required"}
	}
	for _, capability := range next.code.image.Capabilities {
		if _, installed := i.hostCapabilities[capability]; !installed {
			i.vm.leaveOwner()
			return nil, PatchError{Code: "capability_unavailable", Message: fmt.Sprintf("host capability %q is not installed", capability)}
		}
	}
	plan, err := buildPatchPlan(current, next)
	if err != nil {
		i.vm.leaveOwner()
		return nil, err
	}
	if err := i.vm.checkRevisionCapacity(); err != nil {
		i.vm.leaveOwner()
		return nil, err
	}
	plan.owner = i
	plan.state = patchPlanReady
	plan.baseGeneration = current.generation
	i.patchMu.Lock()
	i.pendingPatch = plan
	i.patchMu.Unlock()
	i.vm.leaveOwner()
	return plan, nil
}

// ApplyPatch atomically changes the revision used by subsequent named calls.
func (i *Instance) ApplyPatch(plan *PatchPlan) (PatchResult, error) {
	if i == nil || i.vm == nil {
		return PatchResult{}, PatchError{Code: "closed", Message: "instance is closed"}
	}
	if plan == nil {
		return PatchResult{}, PatchError{Code: "invalid_plan", Message: "patch plan is required"}
	}
	plan.mu.Lock()
	if plan.owner != i || plan.state != patchPlanReady || plan.target == nil {
		plan.mu.Unlock()
		return PatchResult{}, PatchError{Code: "invalid_plan", Message: "patch plan does not belong to this instance"}
	}
	plan.state = patchPlanApplying
	target := plan.target
	baseHash := plan.baseHash
	baseGeneration := plan.baseGeneration
	changedModules := append([]string(nil), plan.changedModules...)
	plan.mu.Unlock()
	fail := func(err error) error {
		plan.mu.Lock()
		if plan.state == patchPlanApplying {
			plan.state = patchPlanReady
		}
		plan.mu.Unlock()
		cleanupErr := plan.Close()
		return errors.Join(err, cleanupErr)
	}
	if err := i.vm.enterOwnerContext(context.Background()); err != nil {
		return PatchResult{}, fail(err)
	}
	defer i.vm.leaveOwner()
	if !i.isOpen() {
		if i.lifecycleState() == instanceFaulted {
			return PatchResult{}, fail(PatchError{Code: "instance_faulted", Message: ErrInstanceFaulted.Error()})
		}
		return PatchResult{}, fail(PatchError{Code: "closed", Message: "instance is closed"})
	}

	current := i.vm.revision.Load()
	if current == nil || current.code.image.Hash != baseHash || current.generation != baseGeneration {
		return PatchResult{}, fail(PatchError{Code: "stale_plan", Message: "instance revision does not match patch base"})
	}
	if err := revisionInitializationError(current); err != nil {
		return PatchResult{}, fail(err)
	}
	if err := i.vm.checkRevisionCapacity(); err != nil {
		return PatchResult{}, fail(err)
	}
	next, err := newInstanceRevision(target, current, current.generation+1)
	if err != nil {
		return PatchResult{}, fail(PatchError{Code: "bind_failed", Message: err.Error()})
	}
	err = bindModuleRequirements(next.root, next.modules)
	if err != nil {
		return PatchResult{}, fail(PatchError{Code: "requirements_failed", Message: err.Error()})
	}

	previousInfo := RevisionInfo{Generation: current.generation, Hash: current.code.image.Hash, SymbolsHash: current.symbolsHash()}
	currentInfo := RevisionInfo{Generation: next.generation, Hash: next.code.image.Hash, SymbolsHash: next.symbolsHash()}
	next.rebindGlobalSlots()
	plan.mu.Lock()
	plan.state = patchPlanCommitted
	plan.owner = nil
	plan.target = nil
	plan.base = nil
	plan.changedModules = nil
	plan.mu.Unlock()
	i.vm.debugger.mu.Lock()
	i.vm.revision.Store(next)
	i.vm.refreshBreakpointsLocked(next)
	i.vm.debugger.mu.Unlock()
	current.retire(true)
	i.vm.addRetiredRevision(current)
	i.vm.sweepRetiredRevisions()
	i.patchMu.Lock()
	if i.pendingPatch == plan {
		i.pendingPatch = nil
	}
	i.patchMu.Unlock()
	return PatchResult{
		Previous:       previousInfo,
		Current:        currentInfo,
		ChangedModules: changedModules,
	}, nil
}

func revisionInitializationError(revision *instanceRevision) error {
	if revision == nil || revision.modules == nil {
		return nil
	}
	for path, module := range revision.modules.modules {
		if module != nil && module.state.initState == moduleInitializing {
			return PatchError{Code: "module_initializing", Message: fmt.Sprintf("module %q is initializing", path)}
		}
	}
	return nil
}
