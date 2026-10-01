package bytecode

import "fmt"

type ValidationLimits struct {
	MaxTypes               int
	MaxConstants           int
	MaxGlobals             int
	MaxFunctions           int
	MaxExports             int
	MaxRequirements        int
	MaxInstructions        int
	MaxLocalsPerFunction   int
	MaxUpvaluesPerFunction int
	MaxPayloadBytes        int
	MaxConstantBytes       int
}

func DefaultValidationLimits() ValidationLimits {
	return defaultValidationLimits
}

func validateArtifactLimits(a *Artifact, limits ValidationLimits) error {
	if err := validateCountLimit("types", len(a.TypeTable.Nodes), limits.MaxTypes); err != nil {
		return err
	}
	if err := validateCountLimit("constants", len(a.Constants), limits.MaxConstants); err != nil {
		return err
	}
	if err := validateCountLimit("globals", len(a.Globals), limits.MaxGlobals); err != nil {
		return err
	}
	if err := validateCountLimit("functions", len(a.Functions), limits.MaxFunctions); err != nil {
		return err
	}
	if err := validateCountLimit("exports", len(a.Exports), limits.MaxExports); err != nil {
		return err
	}
	if err := validateCountLimit("requirements", len(a.Requirements), limits.MaxRequirements); err != nil {
		return err
	}
	totalInstructions := 0
	totalPayloadBytes := 0
	for i, fn := range a.Functions {
		if limits.MaxLocalsPerFunction > 0 && len(fn.Locals) > limits.MaxLocalsPerFunction {
			return validateCountLimit(fmt.Sprintf("functions[%d].locals", i), len(fn.Locals), limits.MaxLocalsPerFunction)
		}
		if limits.MaxUpvaluesPerFunction > 0 && len(fn.Upvalues) > limits.MaxUpvaluesPerFunction {
			return validateCountLimit(fmt.Sprintf("functions[%d].upvalues", i), len(fn.Upvalues), limits.MaxUpvaluesPerFunction)
		}
		if fn.Code != nil {
			totalInstructions += len(fn.Code.Instructions)
			totalPayloadBytes += fn.Code.Descriptors.Bytes() + len(fn.Code.Instructions)*12 + len(fn.Code.Types)*32
			if limits.MaxLocalsPerFunction > 0 && len(fn.Code.Types) > limits.MaxLocalsPerFunction {
				return validateCountLimit(fmt.Sprintf("functions[%d].code.types", i), len(fn.Code.Types), limits.MaxLocalsPerFunction)
			}
			for _, operands := range fn.Code.Operands {
				totalPayloadBytes += len(operands.Inputs)*8 + (len(operands.Outputs)+len(operands.Release)+len(operands.ReleaseBefore))*4
			}
		}
	}
	if err := validateCountLimit("instructions", totalInstructions, limits.MaxInstructions); err != nil {
		return err
	}
	if err := validateCountLimit("instruction_payload_bytes", totalPayloadBytes, limits.MaxPayloadBytes); err != nil {
		return err
	}
	totalConstantBytes := 0
	for i, constant := range a.Constants {
		totalConstantBytes += len(constant.Value)
		if limits.MaxConstantBytes > 0 && len(constant.Value) > limits.MaxConstantBytes {
			return validateCountLimit(fmt.Sprintf("constants[%d].value_bytes", i), len(constant.Value), limits.MaxConstantBytes)
		}
	}
	return validateCountLimit("constant_value_bytes", totalConstantBytes, limits.MaxConstantBytes)
}

func validateCountLimit(path string, got, limit int) error {
	if limit <= 0 || got <= limit {
		return nil
	}
	return newCodedValidationError(ValidationLimitExceeded, path, fmt.Errorf("limit exceeded: got %d, max %d", got, limit))
}
