package bytecode

import "github.com/d7z-team/mini-go/compiler/types"

type ConstPayload struct {
	Constant string `json:"constant"`
}

type LocalPayload struct {
	Local  string `json:"local"`
	Rebind bool   `json:"rebind,omitempty"`
}

type UpvaluePayload struct {
	Upvalue string `json:"upvalue"`
}

type GlobalPayload struct {
	Global string `json:"global"`
}

type AddressPayload struct {
	ModulePath string               `json:"module_path,omitempty"`
	Export     string               `json:"export,omitempty"`
	Kind       string               `json:"kind"`
	Local      string               `json:"local,omitempty"`
	Upvalue    string               `json:"upvalue,omitempty"`
	Global     string               `json:"global,omitempty"`
	Path       []AddressPathSegment `json:"path,omitempty"`
}

type AddressPathSegment struct {
	Kind  string `json:"kind"`
	Field string `json:"field,omitempty"`
	Local string `json:"local,omitempty"`
}

type TypePayload struct {
	Type types.TypeRef `json:"type"`
}

type OperatorPayload struct {
	Operator string `json:"operator"`
}

type MakeSequencePayload struct {
	Type         types.TypeRef `json:"type"`
	ElementCount int           `json:"element_count"`
}

type MakeMapPayload struct {
	Type        types.TypeRef `json:"type"`
	EntryCount  int           `json:"entry_count"`
	HasCapacity bool          `json:"has_capacity,omitempty"`
}

type MakeStructPayload struct {
	Type   types.TypeRef `json:"type"`
	Fields []string      `json:"fields,omitempty"`
}

type MakeSlicePayload struct {
	Type        types.TypeRef `json:"type"`
	HasCapacity bool          `json:"has_capacity,omitempty"`
}

type MakeWaitablePayload struct {
	Type types.TypeRef `json:"type"`
}

// SelectPayload commits one communication and stores its index and receive
// results in locals. A default selection stores index -1.
type SelectPayload struct {
	Index   string       `json:"index"`
	Default bool         `json:"default,omitempty"`
	Cases   []SelectCase `json:"cases,omitempty"`
}

type SelectCase struct {
	Channel string `json:"channel"`
	Send    string `json:"send,omitempty"`
	Value   string `json:"value,omitempty"`
	OK      string `json:"ok,omitempty"`
}

type CountPayload struct {
	Count  int  `json:"count"`
	Expand bool `json:"expand,omitempty"`
}

type FieldPayload struct {
	Field string `json:"field"`
}

type ExportPayload struct {
	ModulePath string `json:"module_path"`
	Export     string `json:"export"`
}

type InitModulePayload struct {
	ModulePath string `json:"module_path"`
}

type LabelPayload struct {
	Label string `json:"label"`
}

type JumpPayload struct {
	Label string `json:"label"`
}

type CallPayload struct {
	ModulePath  string `json:"module_path,omitempty"`
	Function    string `json:"function,omitempty"`
	ArgCount    int    `json:"arg_count"`
	ResultCount int    `json:"result_count,omitempty"`
}

type CallInterfacePayload struct {
	InterfaceType types.TypeRef `json:"interface_type"`
	Method        string        `json:"method"`
	ArgCount      int           `json:"arg_count"`
	ResultCount   int           `json:"result_count,omitempty"`
}

type ClosurePayload struct {
	ModulePath string           `json:"module_path,omitempty"`
	Function   string           `json:"function"`
	Captures   []AddressPayload `json:"captures,omitempty"`
}

type ReturnPayload struct {
	ResultCount int `json:"result_count"`
}

type DeferPayload struct {
	OwnerDepth int `json:"owner_depth,omitempty"`
}

// CallFFIPayload describes the fixed route and byte payload stack contract.
type CallFFIPayload struct {
	ArgCount    int `json:"arg_count"`
	ResultCount int `json:"result_count"`
}

type CallIntrinsicPayload struct {
	ID          IntrinsicID `json:"id"`
	ArgCount    int         `json:"arg_count"`
	ResultCount int         `json:"result_count,omitempty"`
}
