package runtime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/d7z-team/mini-go/compiler/types"
)

func methodIdentity(owner, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if isExportedName(name) {
		return name
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return name
	}
	return owner + "." + name
}

func (m *moduleInstance) valueMethodSet(valueType string) map[string]string {
	valueType = strings.TrimSpace(valueType)
	revision := uint64(0)
	if m.registry != nil {
		revision = m.registry.revision
	}
	if cached, ok := m.valueMethodSetCache.load(valueType); ok && cached.revision == revision {
		return cached.methods
	}
	out := map[string]string{}
	for name, signature := range m.declaredMethodSet(valueType) {
		out[name] = signature
	}
	for name, signature := range m.promotedMethodSet(valueType, map[string]struct{}{}) {
		if _, exists := out[name]; !exists {
			out[name] = signature
		}
	}

	m.valueMethodSetCache.store(valueType, methodSetResolution{methods: out, revision: revision})
	return out
}

// declaredMethodSet derives type conformance from immutable type metadata.
// Executable functions are consulted only when a retained call is dispatched.
func (m *moduleInstance) declaredMethodSet(valueType string) map[string]string {
	valueType = strings.TrimSpace(valueType)
	revision := uint64(0)
	if m.registry != nil {
		revision = m.registry.revision
	}
	if cached, ok := m.declaredMethodSetCache.load(valueType); ok && cached.revision == revision {
		return cached.methods
	}
	out := map[string]string{}
	module, typeName, pointerReceiver, ok := m.methodSetOwner(valueType)
	if !ok {
		m.cacheDeclaredMethodSet(valueType, revision, out)
		return out
	}
	decl, ok := module.executable.Artifact.TypeTable.Named(types.TypeKey{ModulePath: module.modulePath(), DeclID: types.DeclID(typeName)})
	if !ok {
		m.cacheDeclaredMethodSet(valueType, revision, out)
		return out
	}
	for _, method := range decl.Methods {
		receiver := module.localizeType(types.FormatWithTable(&module.executable.Artifact.TypeTable, method.Receiver))
		if isPointerType(receiver) && !pointerReceiver {
			continue
		}
		name := strings.TrimSpace(method.Name)
		signature := module.qualifyLocalType(module.localizeType(types.FormatSignature(&module.executable.Artifact.TypeTable, method.Signature)))
		if name == "" || signature == "" {
			continue
		}
		owner := strings.TrimSpace(method.ModulePath)
		if owner == "" {
			owner = module.modulePath()
		}
		out[methodIdentity(owner, name)] = signature
	}
	m.cacheDeclaredMethodSet(valueType, revision, out)
	return out
}

func (m *moduleInstance) cacheDeclaredMethodSet(valueType string, revision uint64, methods map[string]string) {
	m.declaredMethodSetCache.store(valueType, methodSetResolution{methods: methods, revision: revision})
}

type promotedRuntimeMethodCandidate struct {
	depth     int
	signature string
	count     int
}

type promotedRuntimeMethodFunctionCandidate struct {
	depth       int
	receiver    vmValue
	module      *moduleInstance
	functionID  string
	signature   string
	receiverTyp string
	count       int
}

func (m *moduleInstance) resolveInterfaceMethod(receiver vmValue, interfaceType, method string) (vmValue, *moduleInstance, string, error) {
	interfaceType = strings.TrimSpace(interfaceType)
	method = strings.TrimSpace(method)
	if interfaceType == "" {
		return vmValue{}, nil, "", errors.New("missing interface type")
	}
	if method == "" {
		return vmValue{}, nil, "", errors.New("missing interface method")
	}
	methods := m.interfaceMethods(interfaceType, map[string]struct{}{})
	expectedSignature, ok := methods[methodIdentity(m.modulePath(), method)]
	if !ok {
		return vmValue{}, nil, "", fmt.Errorf("interface %s has no method %s", interfaceType, method)
	}
	asserted, err := m.coerceAssignableValue(receiver, interfaceType)
	if err != nil {
		return vmValue{}, nil, "", err
	}
	dynamic := asserted
	for m.isInterfaceType(dynamic.Type) {
		if dynamic.Data == nil {
			return vmValue{}, nil, "", fmt.Errorf("nil interface method call: %s.%s", interfaceType, method)
		}
		inner, ok := dynamic.Data.(vmValue)
		if !ok {
			return vmValue{}, nil, "", fmt.Errorf("invalid interface value: type=%s data=%T", dynamic.Type, dynamic.Data)
		}
		dynamic = inner
	}
	methodModule, functionID, actualSignature, receiverType, ok := m.methodFunction(dynamic.Type, method)
	if !ok {
		var promotedReceiver vmValue
		promotedReceiver, methodModule, functionID, actualSignature, ok = m.promotedMethodFunction(dynamic, method, map[string]struct{}{})
		if !ok {
			return vmValue{}, nil, "", fmt.Errorf("%s does not implement method %s", dynamic.Type, method)
		}
		dynamic = promotedReceiver
	} else {
		dynamic, err = projectMethodReceiver(dynamic, methodModule, receiverType)
		if err != nil {
			return vmValue{}, nil, "", err
		}
	}
	if actualSignature != expectedSignature {
		return vmValue{}, nil, "", fmt.Errorf("method %s signature mismatch: got %s, want %s", method, actualSignature, expectedSignature)
	}
	return dynamic, methodModule, functionID, nil
}

func (m *moduleInstance) methodFunction(valueType any, method string) (*moduleInstance, string, string, string, bool) {
	if m == nil || m.executable == nil {
		return nil, "", "", "", false
	}
	typeText := strings.TrimSpace(m.resolvedRuntimeType(valueType).String())
	method = strings.TrimSpace(method)
	if typeText == "" || method == "" {
		return nil, "", "", "", false
	}
	revision := uint64(0)
	if m.registry != nil {
		revision = m.registry.revision
	}
	cacheKey := typeText + "\x00" + method
	if cached, ok := m.methodFunctionCache.load(cacheKey); ok && cached.revision == revision {
		return cached.module, cached.functionID, cached.signature, cached.receiverType, cached.found
	}
	module, receiverTypes := m.methodReceiverTypes(typeText)
	for _, receiver := range receiverTypes {
		functionID := "method." + receiver + "." + method
		fn, ok := module.executable.Functions[functionID]
		if !ok {
			continue
		}
		methodSignature := module.localizeType(types.FormatSignature(&module.executable.Artifact.TypeTable, fn.Decl.Signature))
		signature, ok := methodInterfaceSignature(receiver, methodSignature)
		if !ok {
			continue
		}
		signature = module.qualifyLocalType(signature)

		m.methodFunctionCache.store(cacheKey, methodFunctionResolution{
			module: module, functionID: functionID, signature: signature,
			receiverType: receiver, revision: revision, found: true,
		})
		return module, functionID, signature, receiver, true
	}

	m.methodFunctionCache.store(cacheKey, methodFunctionResolution{revision: revision})
	return nil, "", "", "", false
}

func (m *moduleInstance) promotedMethodSet(valueType string, seen map[string]struct{}) map[string]string {
	out := map[string]string{}
	module, typeName, pointerReceiver, ok := m.methodSetOwner(valueType)
	if !ok {
		return out
	}
	if seen == nil {
		seen = map[string]struct{}{}
	}
	candidates := map[string]promotedRuntimeMethodCandidate{}
	module.collectEmbeddedPromotedMethodCandidates(typeName, pointerReceiver, 1, seen, candidates)
	for name, candidate := range candidates {
		if candidate.count == 1 {
			out[name] = candidate.signature
		}
	}
	return out
}

func mergePromotedRuntimeMethodCandidate(candidates map[string]promotedRuntimeMethodCandidate, name, signature string, depth int) {
	name = strings.TrimSpace(name)
	signature = strings.TrimSpace(signature)
	if name == "" || signature == "" || depth <= 0 {
		return
	}
	current, ok := candidates[name]
	if !ok || depth < current.depth {
		candidates[name] = promotedRuntimeMethodCandidate{depth: depth, signature: signature, count: 1}
		return
	}
	if depth == current.depth {
		current.count++
		candidates[name] = current
	}
}

func (m *moduleInstance) collectEmbeddedPromotedMethodCandidates(typeName string, pointerReceiver bool, depth int, seen map[string]struct{}, candidates map[string]promotedRuntimeMethodCandidate) {
	if m == nil || m.executable == nil {
		return
	}
	decl, ok := m.executable.Types[typeName]
	if !ok {
		return
	}
	for _, field := range m.executable.typeFields(decl) {
		if !field.Embedded {
			continue
		}
		embeddedType := m.qualifyLocalType(m.formatType(field.Type))
		if pointerReceiver || isPointerType(embeddedType) {
			if !isPointerType(embeddedType) {
				embeddedType = "Ptr<" + strings.TrimSpace(embeddedType) + ">"
			}
		}
		m.collectMethodSetCandidates(embeddedType, depth, seen, candidates)
	}
}

func (m *moduleInstance) collectMethodSetCandidates(valueType string, depth int, seen map[string]struct{}, candidates map[string]promotedRuntimeMethodCandidate) {
	module, typeName, pointerReceiver, ok := m.methodSetOwner(valueType)
	if !ok {
		return
	}
	key := module.executable.Artifact.Module.Path + ":" + typeName
	if pointerReceiver {
		key = "ptr:" + key
	}
	if _, recursive := seen[key]; recursive {
		return
	}
	seen[key] = struct{}{}
	defer delete(seen, key)
	for name, signature := range module.declaredMethodSet(valueType) {
		mergePromotedRuntimeMethodCandidate(candidates, name, signature, depth)
	}
	module.collectEmbeddedPromotedMethodCandidates(typeName, pointerReceiver, depth+1, seen, candidates)
}

func (m *moduleInstance) promotedMethodFunction(value vmValue, method string, seen map[string]struct{}) (vmValue, *moduleInstance, string, string, bool) {
	candidate := promotedRuntimeMethodFunctionCandidate{}
	m.collectPromotedMethodFunctionCandidates(value, method, 1, seen, &candidate)
	if candidate.count != 1 {
		return vmValue{}, nil, "", "", false
	}
	projected, err := projectMethodReceiver(candidate.receiver, candidate.module, candidate.receiverTyp)
	if err != nil {
		return vmValue{}, nil, "", "", false
	}
	return projected, candidate.module, candidate.functionID, candidate.signature, true
}

func mergePromotedRuntimeMethodFunctionCandidate(candidate *promotedRuntimeMethodFunctionCandidate, depth int, receiver vmValue, module *moduleInstance, functionID, signature, receiverType string) {
	if candidate == nil || depth <= 0 || module == nil || strings.TrimSpace(functionID) == "" || strings.TrimSpace(signature) == "" {
		return
	}
	if candidate.count == 0 || depth < candidate.depth {
		*candidate = promotedRuntimeMethodFunctionCandidate{
			depth:       depth,
			receiver:    receiver,
			module:      module,
			functionID:  functionID,
			signature:   signature,
			receiverTyp: receiverType,
			count:       1,
		}
		return
	}
	if depth == candidate.depth {
		candidate.count++
	}
}

func (m *moduleInstance) collectPromotedMethodFunctionCandidates(value vmValue, method string, depth int, seen map[string]struct{}, candidate *promotedRuntimeMethodFunctionCandidate) {
	module, typeName, pointerReceiver, ok := m.methodSetOwner(value.Type)
	if !ok {
		return
	}
	key := module.executable.Artifact.Module.Path + ":" + typeName
	if pointerReceiver {
		key = "ptr:" + key
	}
	if _, recursive := seen[key]; recursive {
		return
	}
	seen[key] = struct{}{}
	defer delete(seen, key)
	container := value
	if container.Type.ShapeKind() == types.Pointer {
		loaded, err := derefPointer(container)
		if err != nil {
			return
		}
		container = loaded
	}
	if _, ok := container.Data.(*vmStruct); !ok {
		return
	}
	decl, ok := module.executable.Types[typeName]
	if !ok {
		return
	}
	for _, field := range module.executable.typeFields(decl) {
		if !field.Embedded {
			continue
		}
		fieldName := strings.TrimSpace(field.Name)
		embedded, err := loadFieldValue(module, container, fieldName)
		if err != nil {
			continue
		}
		embeddedType := module.qualifyLocalType(module.formatType(field.Type))
		if pointerReceiver && !isPointerType(embeddedType) {
			embedded = embeddedFieldPointer(module, value, fieldName, embeddedType)
		}
		methodModule, functionID, signature, receiverType, ok := module.methodFunction(embedded.Type, method)
		if ok {
			mergePromotedRuntimeMethodFunctionCandidate(candidate, depth, embedded, methodModule, functionID, signature, receiverType)
		}
		module.collectPromotedMethodFunctionCandidates(embedded, method, depth+1, seen, candidate)
	}
}

func projectMethodReceiver(value vmValue, methodModule *moduleInstance, receiverType string) (vmValue, error) {
	receiverType = strings.TrimSpace(receiverType)
	if receiverType == "" {
		return vmValue{}, errors.New("missing method receiver type")
	}
	if coerceRuntimeType(receiverType).ShapeKind() == types.Pointer || value.Type.ShapeKind() != types.Pointer {
		return value, nil
	}
	projected, err := derefPointer(value)
	if err != nil {
		return vmValue{}, err
	}
	if methodModule != nil {
		projected.Type = coerceRuntimeType(methodModule.qualifyLocalType(receiverType))
	}
	return projected, nil
}

func (m *moduleInstance) methodSetOwner(valueType any) (*moduleInstance, string, bool, bool) {
	module := m
	typeText := strings.TrimSpace(m.resolvedRuntimeType(valueType).String())
	pointerReceiver := false
	if elem, ok := m.resolvedRuntimeType(valueType).PointerElem(); ok {
		pointerReceiver = true
		typeText = elem.String()
	}
	if target, name, ok := m.qualifiedTypeModule(typeText); ok {
		module = target
		typeText = name
	}
	if module == nil || module.executable == nil {
		return nil, "", false, false
	}
	if _, ok := module.executable.Types[typeText]; !ok {
		return nil, "", false, false
	}
	return module, typeText, pointerReceiver, true
}

func embeddedFieldPointer(module *moduleInstance, container vmValue, fieldName, fieldType string) vmValue {
	identity := ""
	if container.Type.ShapeKind() == types.Pointer {
		identity = fmt.Sprintf("field:%v:%s", referenceComparableIdentity(container.Data), fieldName)
	}
	return newTargetPointer(&vmPointer{Type: coerceRuntimeType(fieldType), Identity: identity, target: pointerField, module: module, parent: container, field: fieldName})
}

func isPointerType(typ string) bool {
	_, ok := coerceRuntimeType(typ).PointerElem()
	return ok
}

func methodInterfaceSignature(receiver, signature string) (string, bool) {
	params, result, ok := parseFunctionSignatureParts(signature)
	if !ok || len(params) == 0 {
		return "", false
	}
	if strings.TrimSpace(params[0]) != strings.TrimSpace(receiver) {
		return "", false
	}
	return "function(" + strings.Join(params[1:], ", ") + ") " + result, true
}

const canonicalVariadicFunctionParam = "variadic "

func splitCanonicalFunctionParam(param string) string {
	param = strings.TrimSpace(param)
	if strings.HasPrefix(param, canonicalVariadicFunctionParam) {
		return strings.TrimSpace(strings.TrimPrefix(param, canonicalVariadicFunctionParam))
	}
	return param
}

func parseFunctionSignatureParts(signature string) ([]string, string, bool) {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return nil, "", false
	}
	table := &types.TypeTable{}
	parser := types.NewParser("", table)
	ref, err := parser.Parse(signature)
	if err != nil {
		return nil, "", false
	}
	node, ok := table.Node(ref)
	if !ok || node.Kind != types.Function || node.Signature == nil {
		return nil, "", false
	}
	params := make([]string, len(node.Signature.Params))
	for i, param := range node.Signature.Params {
		params[i] = formatRuntimeTypeRef(table, param.Type, map[types.TypeID]bool{})
		if node.Signature.Variadic && i == len(params)-1 {
			params[i] = canonicalVariadicFunctionParam + params[i]
		}
	}
	result := "Void"
	if len(node.Signature.Results) == 1 {
		result = formatRuntimeTypeRef(table, node.Signature.Results[0], map[types.TypeID]bool{})
	} else if len(node.Signature.Results) > 1 {
		results := make([]string, len(node.Signature.Results))
		for i, resultRef := range node.Signature.Results {
			results[i] = formatRuntimeTypeRef(table, resultRef, map[types.TypeID]bool{})
		}
		result = "tuple(" + strings.Join(results, ", ") + ")"
	}
	return params, result, true
}
