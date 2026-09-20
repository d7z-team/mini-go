package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// RetainedRevisions reports published revisions still owned by the VM.
// Prepared candidates are reported separately by RevisionRetention.
func (i *Instance) RetainedRevisions(ctx context.Context) ([]RevisionInfo, error) {
	snapshot, err := i.RevisionRetention(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RevisionInfo, len(snapshot.Revisions))
	for index, retained := range snapshot.Revisions {
		out[index] = retained.Revision
	}
	return out, nil
}

// RevisionRetention describes VM-owned retention, not physical heap ownership.
type RevisionRetention struct {
	Revision   RevisionInfo
	Current    bool
	Pins       int
	GlobalRoot bool // retained by the most recent global-root census
}

// RevisionRetentionSnapshot separates published revisions from an uncommitted target.
type RevisionRetentionSnapshot struct {
	Revisions     []RevisionRetention
	PendingTarget *ProgramIdentity
}

func (i *Instance) RevisionRetention(ctx context.Context) (RevisionRetentionSnapshot, error) {
	var out RevisionRetentionSnapshot
	if i == nil || i.vm == nil {
		return out, errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := i.vm.enterOwnerContext(ctx); err != nil {
		return out, err
	}
	defer i.vm.leaveOwner()
	current := i.vm.revision.Load()
	for _, revision := range append(i.vm.retiredRevisionSnapshot(), current) {
		if revision == nil {
			continue
		}
		revision.mu.Lock()
		if !revision.closed && revision.code != nil {
			out.Revisions = append(out.Revisions, RevisionRetention{
				Revision: RevisionInfo{Generation: revision.generation, Hash: revision.code.image.Hash, SymbolsHash: revision.symbolsHash()},
				Current:  revision == current, Pins: revision.pins, GlobalRoot: revision.rootRetained,
			})
		}
		revision.mu.Unlock()
	}
	i.patchMu.Lock()
	plan := i.pendingPatch
	i.patchMu.Unlock()
	if plan != nil {
		plan.mu.Lock()
		if plan.target != nil {
			identity := plan.target.Identity()
			out.PendingTarget = &identity
		}
		plan.mu.Unlock()
	}
	sort.Slice(out.Revisions, func(a, b int) bool {
		return out.Revisions[a].Revision.Generation < out.Revisions[b].Revision.Generation
	})
	return out, nil
}

type RevisionRootLimits struct {
	MaxNodes int
	MaxDepth int
	MaxRoots int
}

// RevisionRoot identifies a root and a reachable code reference. Paths are
// snapshot-local and omit guest values; repeated roots need not be distinct pins.
type RevisionRoot struct {
	Path     string
	TaskID   int64
	ScopeID  int64
	Function string
	Module   string
	Location Location
}

type RevisionRootSnapshot struct {
	Generation   uint64
	Roots        []RevisionRoot
	ScannedNodes int
	Complete     bool
}

// RevisionRoots traverses VM roots within explicit bounds without changing GC state.
func (i *Instance) RevisionRoots(ctx context.Context, generation uint64, limits RevisionRootLimits) (RevisionRootSnapshot, error) {
	out := RevisionRootSnapshot{Generation: generation, Complete: true}
	if i == nil || i.vm == nil {
		return out, errors.New("instance is closed")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if limits.MaxNodes == 0 {
		limits.MaxNodes = 10000
	}
	if limits.MaxDepth == 0 {
		limits.MaxDepth = 64
	}
	if limits.MaxRoots == 0 {
		limits.MaxRoots = 100
	}
	if limits.MaxNodes < 1 || limits.MaxDepth < 1 || limits.MaxDepth > 256 || limits.MaxRoots < 1 {
		return out, errors.New("invalid revision root limits (depth must be 1..256)")
	}
	if err := i.vm.enterOwnerContext(ctx); err != nil {
		return out, err
	}
	defer i.vm.leaveOwner()
	vm := i.vm
	stopped := false
	step := func() bool {
		if stopped {
			return false
		}
		if ctx.Err() != nil || out.ScannedNodes >= limits.MaxNodes {
			out.Complete, stopped = false, true
			return false
		}
		out.ScannedNodes++
		return true
	}
	add := func(revision *instanceRevision, root RevisionRoot) {
		if revision == nil || revision.generation != generation || stopped {
			return
		}
		if len(out.Roots) == limits.MaxRoots {
			out.Complete, stopped = false, true
			return
		}
		out.Roots = append(out.Roots, root)
	}
	inspect := func(root RevisionRoot, value vmValue) {
		if stopped {
			return
		}
		depth := 0
		var values []vmValue
		walker := newRuntimeValueWalker(func(revision *instanceRevision) {
			ref := root
			if len(values) != 0 {
				ref.Function, ref.Module, ref.Location = "", "", Location{}
				switch function := values[len(values)-1].Data.(type) {
				case functionRef:
					ref.Function = function.FunctionID
					if function.exact != nil {
						ref.Module = function.exact.modulePath()
					}
				case reflectMethodTarget:
					ref.Function, ref.Module = function.Method.FunctionID, function.Method.ModulePath
				case reflectUnboundMethodTarget:
					ref.Function, ref.Module = function.Method.FunctionID, function.Method.ModulePath
				case reflectMakeFuncTarget:
					ref.Function = function.handler.FunctionID
					if function.handlerModule != nil {
						ref.Module = function.handlerModule.modulePath()
					}
				}
				if location, ok := revision.symbols.nearestLocation(ref.Module, ref.Function, 0); ok {
					ref.Location = Location{File: location.File, Line: location.Line, Column: location.Column}
				}
			}
			add(revision, ref)
		})
		walker.enter = func(value vmValue) bool {
			if !step() {
				walker.stopped = true
				return false
			}
			if depth >= limits.MaxDepth {
				out.Complete = false
				return false
			}
			depth++
			values = append(values, value)
			return true
		}
		walker.leave = func() { depth--; values = values[:len(values)-1]; walker.stopped = stopped }
		walker.value(value)
	}
	if current := vm.revision.Load(); current != nil {
		if step() {
			add(current, RevisionRoot{Path: "current"})
		}
		for path, module := range current.modules.modules {
			if !step() {
				break
			}
			for name, cell := range module.state.globals {
				if !step() {
					break
				}
				if value, initialized := cell.snapshot(); initialized {
					inspect(RevisionRoot{Path: "global " + path + "." + name, Module: path}, value)
				}
			}
		}
	}
	if machine := vm.machine; machine != nil {
		for _, task := range machine.tasks {
			if !step() {
				break
			}
			root := RevisionRoot{TaskID: task.id}
			if task.scope != nil {
				root.ScopeID = task.scope.id
			}
			for index, active := range task.retainedFrames {
				if !step() {
					break
				}
				if active == nil || active.frame == nil {
					continue
				}
				frame := active.frame
				root.Path = fmt.Sprintf("task %d/frame %d", task.id, index)
				root.Function = frame.function.Decl.ID
				root.Module = frame.module.modulePath()
				root.Location = Location{}
				if location := runtimeLocation(frame, root.Function, max(0, frame.pc-1)); location != nil {
					root.Location = Location{File: location.File, Line: location.Line, Column: location.Column}
				}
				add(frame.revision, root)
				for _, group := range []struct {
					name  string
					cells []*slot
				}{{"local", frame.localCells}, {"capture", frame.upvalueCells}} {
					for n, cell := range group.cells {
						if !step() {
							break
						}
						value, initialized := cell.snapshot()
						if !initialized {
							continue
						}
						ref := root
						ref.Path += fmt.Sprintf("/%s %d", group.name, n)
						inspect(ref, value)
					}
				}
				for _, group := range []struct {
					name   string
					values []vmValue
				}{{"stack", frame.stack}, {"popped", frame.popValues}, {"return", frame.returnValues}} {
					for n, value := range group.values {
						if stopped {
							break
						}
						ref := root
						ref.Path += fmt.Sprintf("/%s %d", group.name, n)
						inspect(ref, value)
					}
				}
				for n, deferred := range frame.defers {
					if stopped {
						break
					}
					ref := root
					ref.Path += fmt.Sprintf("/defer %d", n)
					inspect(ref, newVMValue("Function", deferred.ref))
				}
				for name, iterator := range frame.mapIterators {
					if stopped {
						break
					}
					ref := root
					ref.Path += "/iterator " + name
					inspect(ref, iterator.object)
				}
				if active.completion != nil {
					ref := root
					ref.Path += "/completion"
					for _, value := range active.completion.returnValues {
						if stopped {
							break
						}
						inspect(ref, value)
					}
					if active.completion.panic != nil {
						inspect(ref, active.completion.panic.err.Value)
					}
				}
				if active.recoveredPanic != nil {
					ref := root
					ref.Path += "/recovered"
					inspect(ref, active.recoveredPanic.err.Value)
				}
			}
			if task.blocked != nil {
				root.Path = fmt.Sprintf("task %d/blocked %s", task.id, task.blocked.kind)
				root.Function = ""
				root.Module = ""
				root.Location = Location{}
				if selection := task.blocked.selection; selection != nil {
					inspect(root, selection.value)
					for index, selected := range selection.cases {
						if stopped {
							break
						}
						ref := root
						ref.Path += fmt.Sprintf("/select/case %d/channel", index)
						inspect(ref, selected.channel)
						ref.Path = root.Path + fmt.Sprintf("/select/case %d/value", index)
						inspect(ref, selected.value)
					}
				}
			}
		}
	}
	for _, timer := range vm.timers {
		if !step() {
			break
		}
		root := RevisionRoot{Path: "timer"}
		if timer.scope != nil {
			root.ScopeID = timer.scope.id
		}
		add(timer.pinnedRevision, root)
		inspect(root, timer.signal)
	}
	for _, value := range vm.reflectTypeValues.snapshot() {
		if stopped {
			break
		}
		inspect(RevisionRoot{Path: "reflection"}, value)
	}
	for _, value := range vm.controlRoots {
		if stopped {
			break
		}
		inspect(RevisionRoot{Path: "allocation"}, value)
	}
	sort.SliceStable(out.Roots, func(a, b int) bool {
		if out.Roots[a].Path != out.Roots[b].Path {
			return out.Roots[a].Path < out.Roots[b].Path
		}
		return out.Roots[a].Function < out.Roots[b].Function
	})
	return out, ctx.Err()
}
