package bytecode

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/d7z-team/mini-go/compiler/token"
	"github.com/d7z-team/mini-go/compiler/types"
)

func validateDefinedType(path string, typ types.TypeNode, refs artifactRefs, table *types.TypeTable) error {
	if typ.ID == "" {
		return missingValidationError(path+".id", errors.New("missing type id"))
	}
	if typ.Identity.DeclID == "" {
		return missingValidationError(path+".identity.decl_id", errors.New("missing type name"))
	}
	if err := validateTypeRef(path+".underlying", typ.Underlying, table); err != nil {
		return err
	}
	methodIdentities := map[string]struct{}{}
	for i, method := range typ.Methods {
		methodPath := path + ".methods[" + strconv.Itoa(i) + "]"
		if strings.TrimSpace(method.Name) == "" {
			return missingValidationError(methodPath+".name", errors.New("missing method name"))
		}
		owner := strings.TrimSpace(method.ModulePath)
		if owner == "" {
			owner = refs.modulePath
		}
		identity := artifactMethodIdentity(owner, method.Name)
		if _, exists := methodIdentities[identity]; exists {
			return newCodedValidationError(ValidationTypeMethodDuplicate, methodPath+".name", fmt.Errorf("duplicate method identity %q", identity))
		}
		methodIdentities[identity] = struct{}{}
		if err := validateTypeRef(methodPath+".receiver", method.Receiver, table); err != nil {
			return err
		}
		if err := validateFunctionSignature(methodPath+".signature", method.Signature, table); err != nil {
			return err
		}
		if strings.TrimSpace(method.FunctionID) != "" {
			modulePath := strings.TrimSpace(method.ModulePath)
			if modulePath != "" && modulePath != refs.modulePath {
				if _, ok := refs.moduleExports[modulePath]; !ok {
					return unknownValidationError(methodPath+".module_path", fmt.Errorf("unknown module requirement %q", modulePath))
				}
			} else if _, ok := refs.functions[method.FunctionID]; !ok {
				return unknownValidationError(methodPath+".function_id", fmt.Errorf("unknown function id %q", method.FunctionID))
			}
		}
	}
	return nil
}

func artifactMethodIdentity(owner, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if token.IsExportedName(name) {
		return name
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return name
	}
	return owner + "." + name
}
