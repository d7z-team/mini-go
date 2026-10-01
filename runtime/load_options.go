package runtime

import (
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type LoadOptions struct {
	MaxArtifactBytes int
	MaxDependencies  int
	MaxTypes         int
	MaxFunctions     int
	MaxInstructions  int
}

var defaultLoadOptions = LoadOptions{
	MaxArtifactBytes: 256 << 20,
	MaxDependencies:  4096,
	MaxTypes:         1_000_000,
	MaxFunctions:     1_000_000,
	MaxInstructions:  10_000_000,
}

func normalizeLoadOptions(options LoadOptions) LoadOptions {
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = defaultLoadOptions.MaxArtifactBytes
	}
	if options.MaxDependencies == 0 {
		options.MaxDependencies = defaultLoadOptions.MaxDependencies
	}
	if options.MaxTypes == 0 {
		options.MaxTypes = defaultLoadOptions.MaxTypes
	}
	if options.MaxFunctions == 0 {
		options.MaxFunctions = defaultLoadOptions.MaxFunctions
	}
	if options.MaxInstructions == 0 {
		options.MaxInstructions = defaultLoadOptions.MaxInstructions
	}
	return options
}

func validateArtifactLoad(artifact *ir.Artifact, options LoadOptions) error {
	if len(artifact.TypeTable.Nodes) > options.MaxTypes {
		return fmt.Errorf("artifact type limit exceeded: max %d", options.MaxTypes)
	}
	if len(artifact.Functions) > options.MaxFunctions {
		return fmt.Errorf("artifact function limit exceeded: max %d", options.MaxFunctions)
	}
	instructions := 0
	for _, function := range artifact.Functions {
		if function.Code != nil {
			instructions += len(function.Code.Instructions)
		}
		if instructions > options.MaxInstructions {
			return fmt.Errorf("artifact instruction limit exceeded: max %d", options.MaxInstructions)
		}
	}
	return nil
}
