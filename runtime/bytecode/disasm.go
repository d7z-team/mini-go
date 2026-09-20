package bytecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/d7z-team/mini-go/compiler/types"
)

func Disassemble(a *Artifact) (string, error) {
	var buf bytes.Buffer
	if err := WriteDisassembly(&buf, a); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func WriteDisassembly(w io.Writer, a *Artifact) error {
	if err := ValidateArtifact(a); err != nil {
		return err
	}
	fmt.Fprintf(w, "format %s version %d opcode_set %s\n", a.Format, a.Version, a.OpcodeSet)
	fmt.Fprintf(w, "module %s package %s\n", a.Module.Path, a.Module.Package)
	writeRequirements(w, a.Requirements)
	writeTypes(w, a.TypeTable.DefinedNamed(a.Module.Path), &a.TypeTable)
	writeConstants(w, a.Constants, &a.TypeTable)
	writeGlobals(w, a.Globals, &a.TypeTable)
	writeExports(w, a.Exports, &a.TypeTable)
	writeFunctions(w, a.Functions, &a.TypeTable)
	return nil
}

func writeRequirements(w io.Writer, requirements []Requirement) {
	for _, requirement := range requirements {
		fmt.Fprintf(w, "require %s %s", requirement.Kind, requirement.ModulePath)
		if requirement.Hash != "" {
			fmt.Fprintf(w, " hash %s", requirement.Hash)
		}
		writeStringList(w, " exports", requirement.Exports)
		fmt.Fprintln(w)
	}
}

func writeTypes(w io.Writer, declarations []types.TypeNode, table *types.TypeTable) {
	for _, typ := range declarations {
		name := string(typ.Identity.DeclID)
		fmt.Fprintf(w, "type type.%s %s = %s\n", name, name, types.FormatWithTable(table, typ.Underlying))
		underlying := table.Underlying(types.Ref(typ))
		shape, _ := table.Node(underlying)
		for _, field := range shape.Fields {
			fmt.Fprintf(w, "  field %s %s", field.Name, types.FormatWithTable(table, field.Type))
			if signature, ok := table.IsFunction(field.Type); ok && signature.Variadic {
				fmt.Fprint(w, " [variadic]")
			}
			if field.Tag != "" {
				fmt.Fprintf(w, " tag %s", quoteString(field.Tag))
			}
			fmt.Fprintln(w)
		}
		for _, method := range typ.Methods {
			fmt.Fprintf(w, "  method %s %s %s", method.Name, types.FormatWithTable(table, method.Receiver), types.FormatSignature(table, method.Signature))
			if method.FunctionID != "" {
				fmt.Fprintf(w, " %s", method.FunctionID)
			}
			if method.ModulePath != "" {
				fmt.Fprintf(w, " module %s", method.ModulePath)
			}
			fmt.Fprintln(w)
		}
	}
}

func writeConstants(w io.Writer, constants []Constant, table *types.TypeTable) {
	for _, constant := range constants {
		fmt.Fprintf(w, "const %s %s = %s", constant.ID, types.FormatWithTable(table, constant.Type), compactJSON(constant.Value))
		if constant.Untyped {
			fmt.Fprint(w, " [untyped]")
		}
		fmt.Fprintln(w)
	}
}

func writeGlobals(w io.Writer, globals []Global, table *types.TypeTable) {
	for _, global := range globals {
		fmt.Fprintf(w, "global %s %s", global.ID, types.FormatWithTable(table, global.Type))
		fmt.Fprintln(w)
	}
}

func writeExports(w io.Writer, exports []Export, table *types.TypeTable) {
	for _, export := range exports {
		fmt.Fprintf(w, "export %s %s %s", export.Name, export.Kind, export.ID)
		if export.Type.Valid() {
			fmt.Fprintf(w, " %s", types.FormatWithTable(table, export.Type))
		}
		if export.Untyped {
			fmt.Fprint(w, " untyped")
		}
		fmt.Fprintln(w)
	}
}

func writeFunctions(w io.Writer, functions []Function, table *types.TypeTable) {
	for _, fn := range functions {
		fmt.Fprintf(w, "func %s %s", fn.ID, types.FormatSignature(table, fn.Signature))
		if fn.Signature.Variadic {
			fmt.Fprint(w, " [variadic]")
		}
		fmt.Fprintln(w)
		for _, local := range fn.Locals {
			fmt.Fprintf(w, "  local %s %s", local.ID, types.FormatWithTable(table, local.Type))
			fmt.Fprintln(w)
		}
		for _, local := range fn.ResultLocals {
			fmt.Fprintf(w, "  result_local %s\n", local)
		}
		for _, upvalue := range fn.Upvalues {
			fmt.Fprintf(w, "  upvalue %s %s", upvalue.ID, types.FormatWithTable(table, upvalue.Type))
			fmt.Fprintln(w)
		}
		for i, inst := range fn.Instructions {
			fmt.Fprintf(w, "  %04d %s", i, inst.Op)
			if len(inst.Payload) != 0 {
				fmt.Fprintf(w, " %s", compactJSON(inst.Payload))
			}
			fmt.Fprintln(w)
		}
	}
}

func writeStringList(w io.Writer, label string, values []string) {
	if len(values) == 0 {
		return
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	fmt.Fprintf(w, "%s [", label)
	for i, value := range out {
		if i > 0 {
			fmt.Fprint(w, ",")
		}
		fmt.Fprint(w, value)
	}
	fmt.Fprint(w, "]")
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func quoteString(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(raw)
}
