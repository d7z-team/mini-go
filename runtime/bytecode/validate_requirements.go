package bytecode

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func validateRequirements(requirements []Requirement) error {
	for i, requirement := range requirements {
		path := "requirements[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(requirement.ModulePath) == "" {
			return missingValidationError(path+".module_path", errors.New("missing module path"))
		}
		switch requirement.Kind {
		case RequirementSource:
			if requirement.Hash != "" && !validSHA256(requirement.Hash) {
				return requirementValidationError(path+".hash", errors.New("invalid source artifact hash"))
			}
			seen := make(map[string]struct{}, len(requirement.Exports))
			for j, name := range requirement.Exports {
				if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
					return requirementValidationError(path+".exports["+strconv.Itoa(j)+"]", errors.New("invalid source export"))
				}
				if _, duplicate := seen[name]; duplicate {
					return requirementValidationError(path+".exports["+strconv.Itoa(j)+"]", errors.New("duplicate source export"))
				}
				seen[name] = struct{}{}
			}
		default:
			return newCodedValidationError(ValidationRequirementInvalid, path+".kind", fmt.Errorf("unknown requirement kind %q", requirement.Kind))
		}
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func requirementValidationError(path string, err error) ValidationError {
	return newCodedValidationError(ValidationRequirementInvalid, path, err)
}

func collectModuleExports(requirements []Requirement) map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{})
	for _, requirement := range requirements {
		if requirement.Kind != RequirementSource {
			continue
		}
		modulePath := strings.TrimSpace(requirement.ModulePath)
		if modulePath == "" {
			continue
		}
		exports := out[modulePath]
		if exports == nil {
			exports = make(map[string]struct{})
			out[modulePath] = exports
		}
		for _, export := range requirement.Exports {
			export = strings.TrimSpace(export)
			if export != "" {
				exports[export] = struct{}{}
			}
		}
	}
	return out
}
