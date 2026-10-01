package main

import (
	"fmt"
	"go/build"
	"go/importer"
	"go/token"
	gotypes "go/types"
	"sort"
	"strconv"
	"strings"
)

func buildGoSnapshot(referenceVersion string) (snapshot, error) {
	paths, err := packagePaths()
	if err != nil {
		return snapshot{}, err
	}
	out := snapshot{Schema: schema, Version: version, GoVersion: referenceVersion}
	for _, path := range paths {
		if (path == "encoding/json") != (referenceVersion == jsonGoVersion) {
			continue
		}
		buildPackage, err := build.Default.Import(path, "", build.FindOnly)
		if err != nil || !buildPackage.Goroot {
			continue
		}
		pkg, err := importer.Default().Import(path)
		if err != nil {
			return snapshot{}, fmt.Errorf("import Go package %s: %w", path, err)
		}
		entry := apiPackage{Path: path, Complete: completePackages[path]}
		names := pkg.Scope().Names()
		sort.Strings(names)
		for _, name := range names {
			if !token.IsExported(name) {
				continue
			}
			object := pkg.Scope().Lookup(name)
			decl := apiDeclaration{Name: name, Type: goType(object.Type(), pkg)}
			switch value := object.(type) {
			case *gotypes.Const:
				basic, ok := value.Type().(*gotypes.Basic)
				decl.Kind, decl.Untyped, decl.Exact = "const", ok && basic.Info()&gotypes.IsUntyped != 0, value.Val().ExactString()
			case *gotypes.Var:
				decl.Kind = "var"
			case *gotypes.Func:
				decl.Kind = "func"
				decl.TypeParams = goTypeParameters(value.Signature().TypeParams(), pkg)
			case *gotypes.TypeName:
				decl.Kind, decl.Alias = "type", value.IsAlias()
				decl.Fields, decl.Methods = goTypeMembers(value.Type(), pkg)
				switch typ := value.Type().(type) {
				case *gotypes.Named:
					decl.TypeParams = goTypeParameters(typ.TypeParams(), pkg)
				case *gotypes.Alias:
					decl.TypeParams = goTypeParameters(typ.TypeParams(), pkg)
				}
			default:
				continue
			}
			if !trackedDeclaration(path, decl.Kind, decl.Name) {
				continue
			}
			entry.Declarations = append(entry.Declarations, decl)
		}
		out.Packages = append(out.Packages, entry)
	}
	return out, nil
}

func goTypeParameters(params *gotypes.TypeParamList, owner *gotypes.Package) []apiField {
	if params == nil {
		return nil
	}
	out := make([]apiField, 0, params.Len())
	for index := 0; index < params.Len(); index++ {
		param := params.At(index)
		out = append(out, apiField{Name: param.Obj().Name(), Type: goType(param.Constraint(), owner)})
	}
	return out
}

func goTypeMembers(typ gotypes.Type, owner *gotypes.Package) ([]apiField, []apiField) {
	named, ok := gotypes.Unalias(typ).(*gotypes.Named)
	if !ok {
		return nil, nil
	}
	var fields []apiField
	if structure, ok := named.Underlying().(*gotypes.Struct); ok {
		for index := 0; index < structure.NumFields(); index++ {
			field := structure.Field(index)
			if field.Exported() {
				fields = append(fields, apiField{Name: field.Name(), Type: goType(field.Type(), owner), Tag: structure.Tag(index), Embedded: field.Embedded()})
			}
		}
	}
	methodType := gotypes.Type(gotypes.NewPointer(named))
	if iface, ok := named.Underlying().(*gotypes.Interface); ok {
		iface.Complete()
		methodType = named
	}
	methodSet := gotypes.NewMethodSet(methodType)
	methods := make([]apiField, 0, methodSet.Len())
	for index := 0; index < methodSet.Len(); index++ {
		method := methodSet.At(index).Obj()
		if !method.Exported() {
			continue
		}
		signature := method.Type().(*gotypes.Signature)
		methods = append(methods, apiField{Name: method.Name(), Type: goSignature(signature, owner, false), Variadic: signature.Variadic(), TypeParams: goTypeParameters(signature.TypeParams(), owner)})
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
	return fields, methods
}

func goType(typ gotypes.Type, owner *gotypes.Package) string {
	switch value := typ.(type) {
	case *gotypes.Basic:
		return canonicalBasic(value.Name())
	case interface {
		Obj() *gotypes.TypeName
		TypeArgs() *gotypes.TypeList
	}:
		object := value.Obj()
		if object.Pkg() == nil {
			return canonicalBasic(object.Name())
		}
		name := object.Pkg().Path() + "." + object.Name()
		if value.TypeArgs().Len() == 0 {
			return name
		}
		parts := []string{name}
		for index := 0; index < value.TypeArgs().Len(); index++ {
			parts = append(parts, goType(value.TypeArgs().At(index), owner))
		}
		return "Instance<" + strings.Join(parts, ", ") + ">"
	case *gotypes.Pointer:
		return "Ptr<" + goType(value.Elem(), owner) + ">"
	case *gotypes.Slice:
		return "Slice<" + goType(value.Elem(), owner) + ">"
	case *gotypes.Array:
		return "Array<" + strconv.FormatInt(value.Len(), 10) + ", " + goType(value.Elem(), owner) + ">"
	case *gotypes.Map:
		return "Map<" + goType(value.Key(), owner) + ", " + goType(value.Elem(), owner) + ">"
	case *gotypes.Chan:
		name := "Waitable"
		if value.Dir() == gotypes.SendOnly {
			name = "SendWaitable"
		} else if value.Dir() == gotypes.RecvOnly {
			name = "ReceiveWaitable"
		}
		return name + "<" + goType(value.Elem(), owner) + ">"
	case *gotypes.Signature:
		return goSignature(value, owner, true)
	case *gotypes.Interface:
		value.Complete()
		if value.NumEmbeddeds() == 0 && value.NumMethods() == 0 && value.IsComparable() {
			return "Comparable"
		}
		parts := make([]string, 0, value.NumEmbeddeds()+value.NumMethods())
		for index := 0; index < value.NumEmbeddeds(); index++ {
			parts = append(parts, goType(value.EmbeddedType(index), owner))
		}
		for index := 0; index < value.NumMethods(); index++ {
			method := value.Method(index)
			parts = append(parts, method.Name()+":"+goSignature(method.Type().(*gotypes.Signature), owner, false))
		}
		return "interface{" + strings.Join(parts, ",") + "}"
	case *gotypes.Union:
		parts := make([]string, value.Len())
		for index := range parts {
			term := value.Term(index)
			parts[index] = goType(term.Type(), owner)
			if term.Tilde() {
				parts[index] = "~" + parts[index]
			}
		}
		return strings.Join(parts, "|")
	case *gotypes.Struct:
		parts := make([]string, value.NumFields())
		for index := 0; index < value.NumFields(); index++ {
			parts[index] = value.Field(index).Name() + ":" + goType(value.Field(index).Type(), owner)
		}
		return "struct{" + strings.Join(parts, ",") + "}"
	case *gotypes.Tuple:
		parts := make([]string, value.Len())
		for index := range parts {
			parts[index] = goType(value.At(index).Type(), owner)
		}
		return "Tuple<" + strings.Join(parts, ", ") + ">"
	case *gotypes.TypeParam:
		return value.Obj().Name()
	default:
		return gotypes.TypeString(typ, func(pkg *gotypes.Package) string { return pkg.Path() })
	}
}

func goSignature(signature *gotypes.Signature, owner *gotypes.Package, includeReceiver bool) string {
	params := make([]string, 0, signature.Params().Len()+1)
	if includeReceiver && signature.Recv() != nil {
		params = append(params, goType(signature.Recv().Type(), owner))
	}
	for index := 0; index < signature.Params().Len(); index++ {
		text := goType(signature.Params().At(index).Type(), owner)
		if signature.Variadic() && index == signature.Params().Len()-1 {
			text = "variadic " + text
		}
		params = append(params, text)
	}
	results := make([]string, signature.Results().Len())
	for index := range results {
		results[index] = goType(signature.Results().At(index).Type(), owner)
	}
	out := "function(" + strings.Join(params, ", ") + ")"
	if len(results) == 1 {
		return out + " " + results[0]
	}
	if len(results) > 1 {
		return out + " tuple(" + strings.Join(results, ", ") + ")"
	}
	return out
}

func canonicalBasic(name string) string {
	switch name {
	case "bool":
		return "Bool"
	case "string":
		return "String"
	case "byte", "uint8":
		return "Uint8"
	case "rune", "int32":
		return "Int32"
	case "any":
		return "Any"
	case "error":
		return "interface{Error:function() String}"
	case "untyped bool":
		return "Bool"
	case "untyped string":
		return "String"
	case "untyped rune":
		return "Int32"
	case "untyped int":
		return "Int"
	case "untyped float":
		return "Float64"
	case "untyped complex":
		return "Complex128"
	}
	if name == "uintptr" {
		return "Uintptr"
	}
	if len(name) != 0 {
		return strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}
