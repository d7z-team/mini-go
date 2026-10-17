package runtimecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	goruntime "runtime"
	"sort"

	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

// StateVector records exact owner observations for minimal bytecode inputs.
type StateVector struct {
	Name                 string                  `json:"name"`
	Image                bytecode.ExecutionImage `json:"image"`
	Limit                int64                   `json:"limit"`
	Actions              []StateAction           `json:"actions"`
	InitializationError  string                  `json:"initialization_error,omitempty"`
	InitializationSteps  int64                   `json:"initialization_steps"`
	InitializationMemory [4]int64                `json:"initialization_memory"`
}

type StateAction struct {
	Operation string                 `json:"operation"`
	State     runtime.ExecutionState `json:"state,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Steps     int                    `json:"steps"`
	Memory    [4]int64               `json:"memory"`
}

// GenerateStateVectors runs repeated invocations without compiling source or
// clearing instance state. Every observation comes from the current Go owner.
func GenerateStateVectors(ctx context.Context, inputs map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	var vectors []StateVector
	for _, name := range names {
		var artifacts []bytecode.Artifact
		input := bytes.TrimSpace(inputs[name])
		if len(input) > 0 && input[0] == '[' {
			if err := json.Unmarshal(input, &artifacts); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		} else {
			artifacts = make([]bytecode.Artifact, 1)
			if err := json.Unmarshal(input, &artifacts[0]); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
		if len(artifacts) == 0 {
			return nil, fmt.Errorf("%s: missing root artifact", name)
		}
		module := artifacts[0].Module.Path
		image := bytecode.ExecutionImage{
			Format: bytecode.ExecutionFormat, Version: bytecode.ExecutionVersion,
			CompilerID: bytecode.CompilerIdentity, ContractID: bytecode.ExecutionContract,
			Root: module, Target: target.Target{Tags: []string{"minigo"}},
			Entries:  []bytecode.Entry{{Name: "default", ModulePath: module, FunctionID: "fn.Main"}},
			Packages: make(map[string]bytecode.PackageArchive),
		}
		for _, artifact := range artifacts {
			artifact.Format, artifact.Version, artifact.OpcodeSet = bytecode.Format, bytecode.CurrentVersion, bytecode.OpcodeSet
			hash, err := bytecode.Hash(&artifact)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(artifact)
			if err != nil {
				return nil, err
			}
			if _, exists := image.Packages[artifact.Module.Path]; exists {
				return nil, fmt.Errorf("%s: duplicate module %s", name, artifact.Module.Path)
			}
			image.Packages[artifact.Module.Path] = bytecode.PackageArchive{Artifact: encoded, ArtifactHash: hash}
		}
		var err error
		image.Hash, err = bytecode.HashExecutionImage(image)
		if err != nil {
			return nil, err
		}
		program, err := runtime.LoadExecutionImage(image)
		if err != nil {
			return nil, err
		}
		for _, limit := range []int64{127, 128, 159, 160, 512, 600, 640, 1024, 1536, 2048, 4096} {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			vector := StateVector{Name: name, Image: image, Limit: limit, Actions: []StateAction{}}
			if err := func() error {
				instance, err := program.Instantiate(ctx, runtime.InstanceOptions{Limits: runtime.Limits{MaxGuestBytes: limit, MaxSteps: 1000}})
				if err != nil {
					if err := ctx.Err(); err != nil {
						return err
					}
					projected, projectionErr := runtime.ProjectExecutionError(err)
					if projectionErr != nil {
						return projectionErr
					}
					vector.InitializationError = projected.Code
					return nil
				}
				defer instance.Close()
				initial, err := instance.RuntimeStats(ctx)
				if err != nil {
					return err
				}
				vector.InitializationSteps = initial.ExecutedSteps
				vector.InitializationMemory = [4]int64{initial.OccupiedGuestBytes, initial.ReservedGuestBytes, initial.TotalGuestAllocatedBytes, initial.ResidentPageBytes}
				observe := func(action StateAction, operationErr error) error {
					if operationErr != nil {
						projected, err := runtime.ProjectExecutionError(operationErr)
						if err != nil {
							return err
						}
						action.Error = projected.Code
					}
					stats, err := instance.RuntimeStats(ctx)
					if err != nil {
						return err
					}
					action.Memory = [4]int64{stats.OccupiedGuestBytes, stats.ReservedGuestBytes, stats.TotalGuestAllocatedBytes, stats.ResidentPageBytes}
					vector.Actions = append(vector.Actions, action)
					return nil
				}
				for invocation := range 4 {
					execution, err := instance.Start("default")
					if observeErr := observe(StateAction{Operation: "start"}, err); observeErr != nil {
						return observeErr
					}
					if err != nil {
						continue
					}
					for step := 0; step < 1001; step++ {
						state, count, err := execution.PollSteps(1)
						// The cancellation supervisor may briefly own the VM. Busy
						// performs no guest work and is not a bytecode observation.
						for errors.Is(err, runtime.ErrBusy) {
							if err := ctx.Err(); err != nil {
								return err
							}
							goruntime.Gosched()
							state, count, err = execution.PollSteps(1)
						}
						if observeErr := observe(StateAction{Operation: "poll", State: state, Steps: count}, err); observeErr != nil {
							return observeErr
						}
						if err != nil || state == runtime.ExecutionCompleted {
							break
						}
						if invocation == 2 {
							execution.Cancel()
							if err := observe(StateAction{Operation: "cancel"}, nil); err != nil {
								return err
							}
							break
						}
						if step == 1000 {
							return fmt.Errorf("%s did not terminate within its instruction limit", name)
						}
					}
				}
				return nil
			}(); err != nil {
				return nil, fmt.Errorf("%s limit %d: %w", name, limit, err)
			}
			vectors = append(vectors, vector)
		}
	}
	return json.Marshal(vectors)
}
