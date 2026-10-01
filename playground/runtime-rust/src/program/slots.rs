use super::{Instruction, LOCAL_OUTPUT, PreparedFunction, RuntimeError, instruction_arity};
use crate::{contract_generated as wire, loader::DecodedImage, types::TypeIdentity};

#[derive(Clone, Default)]
pub(crate) struct PreparedOperands {
    offsets: Vec<[usize; 4]>,
    inputs: Vec<wire::Operand>,
    transfers: Vec<u32>,
    outputs: Vec<u32>,
    releases: Vec<u32>,
    entry_releases: Vec<u32>,
}

pub(crate) struct OperandView<'a> {
    pub inputs: &'a [wire::Operand],
    pub outputs: &'a [u32],
    pub release: &'a [u32],
    pub release_before: &'a [u32],
}

impl PreparedOperands {
    pub(super) fn push(&mut self, operands: wire::SlotOperands) {
        // Most instructions have at most two inputs. Avoid allocating maps
        // for those, while bounding work for hostile large argument lists.
        let indexed = (operands.inputs.len() > 8).then(|| {
            let mut occurrences = std::collections::HashMap::<u32, usize>::new();
            for input in operands.inputs.iter().filter(|input| input.kind == 0) {
                *occurrences.entry(input.index).or_default() += 1;
            }
            let released: std::collections::HashSet<_> = operands.release.iter().copied().collect();
            (occurrences, released)
        });
        self.transfers.extend(operands.inputs.iter().map(|input| {
            let transferable = input.kind == 0
                && match &indexed {
                    Some((occurrences, released)) => {
                        occurrences[&input.index] == 1 && released.contains(&input.index)
                    }
                    None => {
                        operands
                            .inputs
                            .iter()
                            .filter(|other| other.kind == 0 && other.index == input.index)
                            .count()
                            == 1
                            && operands.release.contains(&input.index)
                    }
                };
            if transferable { input.index } else { u32::MAX }
        }));
        self.inputs.extend(operands.inputs.0.unwrap_or_default());
        self.outputs.extend(operands.outputs.0.unwrap_or_default());
        self.releases.extend(operands.release.0.unwrap_or_default());
        self.entry_releases
            .extend(operands.release_before.0.unwrap_or_default());
        self.offsets.push([
            self.inputs.len(),
            self.outputs.len(),
            self.releases.len(),
            self.entry_releases.len(),
        ]);
    }

    pub(crate) fn at(&self, pc: usize) -> OperandView<'_> {
        let start = if pc == 0 {
            [0; 4]
        } else {
            self.offsets[pc - 1]
        };
        let end = self.offsets[pc];
        OperandView {
            inputs: &self.inputs[start[0]..end[0]],
            outputs: &self.outputs[start[1]..end[1]],
            release: &self.releases[start[2]..end[2]],
            release_before: &self.entry_releases[start[3]..end[3]],
        }
    }

    // Execution usually needs one storage domain. Constructing the complete
    // view here would check all four unrelated slice bounds on every read.
    pub(crate) fn inputs_at(&self, pc: usize) -> &[wire::Operand] {
        let start = if pc == 0 { 0 } else { self.offsets[pc - 1][0] };
        &self.inputs[start..self.offsets[pc][0]]
    }

    pub(crate) fn outputs_at(&self, pc: usize) -> &[u32] {
        let start = if pc == 0 { 0 } else { self.offsets[pc - 1][1] };
        &self.outputs[start..self.offsets[pc][1]]
    }

    pub(crate) fn transfers_at(&self, pc: usize) -> &[u32] {
        let start = if pc == 0 { 0 } else { self.offsets[pc - 1][0] };
        &self.transfers[start..self.offsets[pc][0]]
    }

    pub(crate) fn release_at(&self, pc: usize) -> &[u32] {
        let start = if pc == 0 { 0 } else { self.offsets[pc - 1][2] };
        &self.releases[start..self.offsets[pc][2]]
    }

    pub(crate) fn release_before_at(&self, pc: usize) -> &[u32] {
        let start = if pc == 0 { 0 } else { self.offsets[pc - 1][3] };
        &self.entry_releases[start..self.offsets[pc][3]]
    }

    pub(super) fn iter(&self) -> impl Iterator<Item = OperandView<'_>> {
        (0..self.offsets.len()).map(|pc| self.at(pc))
    }
}

pub(super) fn verify_types(
    function: &PreparedFunction,
    decoded: &DecodedImage,
    constants: &[TypeIdentity],
    signatures: &std::collections::BTreeMap<(&str, &str), &wire::FunctionSignature>,
    path: &str,
) -> Result<(), RuntimeError> {
    let registry = decoded.types();
    let artifact = &decoded.artifacts()[function.module.as_ref()];
    let operands = &function.operands;
    let boolean = TypeIdentity::Primitive(wire::PrimitiveBool);
    let globals: std::collections::HashMap<_, _> = artifact
        .globals
        .iter()
        .map(|global| (global.id.as_str(), &global.r#type))
        .collect();
    for (pc, instruction) in function.code.iter().enumerate() {
        let operands = operands.at(pc);
        for &output in operands
            .outputs
            .iter()
            .filter(|output| **output & LOCAL_OUTPUT != 0)
        {
            let index = (output & !LOCAL_OUTPUT) as usize;
            let typ = registry.underlying(&function.local_types[index])?;
            if function.addressable_locals[index]
                || !matches!(typ, TypeIdentity::Primitive(kind) if kind == wire::PrimitiveBool || (wire::PrimitiveInt..=wire::PrimitiveComplex128).contains(&kind))
            {
                return Err(RuntimeError::new(
                    "invalid_operand",
                    path,
                    "direct local output requires private fixed-size scalar storage",
                ));
            }
        }
        for operand in operands.inputs.iter().filter(|operand| operand.kind == 2) {
            let index = operand.index as usize;
            let primitive = registry.underlying(&function.local_types[index])?;
            let direct = matches!(primitive, TypeIdentity::Primitive(kind) if (wire::PrimitiveBool..=wire::PrimitiveComplex128).contains(&kind))
                || registry.pointer_element(&primitive)?.is_some();
            let field_read = if matches!(instruction, Instruction::GetPath(_)) {
                let output = operands.outputs[0];
                let typ = if output & LOCAL_OUTPUT != 0 {
                    &function.local_types[(output & !LOCAL_OUTPUT) as usize]
                } else {
                    &function.slot_types[output as usize]
                };
                let result = registry.underlying(typ)?;
                matches!(result, TypeIdentity::Primitive(kind) if (wire::PrimitiveBool..=wire::PrimitiveComplex128).contains(&kind))
                    || registry.pointer_element(&result)?.is_some()
            } else {
                false
            };
            if function.addressable_locals[index] || !(direct || field_read) {
                return Err(RuntimeError::new(
                    "invalid_operand",
                    path,
                    "direct local input requires private scalar, pointer or scalar field read",
                ));
            }
        }
        let input = |index: usize| {
            let operand = &operands.inputs[index];
            match operand.kind {
                0 => &function.slot_types[operand.index as usize],
                2 => &function.local_types[operand.index as usize],
                _ => &constants[operand.index as usize],
            }
        };
        let resolve = |typ: &wire::TypeRef| registry.resolve(&function.module, typ);
        let mut inputs: Option<Vec<TypeIdentity>> = None;
        let mut outputs: Option<Vec<TypeIdentity>> = None;
        let mut integer_inputs = Vec::new();
        use Instruction::*;
        match instruction {
            Len | Cap => outputs = Some(vec![TypeIdentity::Primitive(wire::PrimitiveInt)]),
            LoadLocal(local) => {
                outputs = Some(vec![
                    function.local_types[function.locals[&local.local]].clone(),
                ])
            }
            StoreLocal(local) => {
                inputs = Some(vec![
                    function.local_types[function.locals[&local.local]].clone(),
                ])
            }
            LoadUpvalue(upvalue) | StoreUpvalue(upvalue) => {
                let typ = resolve(
                    &function.declaration.upvalues[function.upvalues[&upvalue.upvalue]].r#type,
                )?;
                if matches!(instruction, LoadUpvalue(_)) {
                    outputs = Some(vec![typ]);
                } else {
                    inputs = Some(vec![typ]);
                }
            }
            LoadGlobal(global) | StoreGlobal(global) => {
                let typ = resolve(globals[global.global.as_str()])?;
                if matches!(instruction, LoadGlobal(_)) {
                    outputs = Some(vec![typ]);
                } else {
                    inputs = Some(vec![typ]);
                }
            }
            Zero(payload) | Convert(payload) | TypeAssert(payload) => {
                outputs = Some(vec![resolve(&payload.r#type)?])
            }
            TypeAssertOk(payload) => {
                outputs = Some(vec![resolve(&payload.r#type)?, boolean.clone()])
            }
            MakeStruct(payload) => {
                let typ = resolve(&payload.r#type)?;
                let (module, node) = registry
                    .node(&typ)?
                    .filter(|(_, node)| node.kind == wire::Struct)
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_operand", path, "construction requires a struct")
                    })?;
                let fields: std::collections::HashMap<_, _> = node
                    .fields
                    .iter()
                    .map(|field| (field.name.as_str(), &field.r#type))
                    .collect();
                inputs = Some(
                    payload
                        .fields
                        .iter()
                        .map(|name| {
                            let field = fields.get(name.as_str()).ok_or_else(|| {
                                RuntimeError::new(
                                    "invalid_operand",
                                    path,
                                    "unknown constructor field",
                                )
                            })?;
                            registry.resolve(module, field)
                        })
                        .collect::<Result<_, _>>()?,
                );
                outputs = Some(vec![typ]);
            }
            MakeSequence(payload) => {
                let typ = resolve(&payload.r#type)?;
                let element = match registry.underlying(&typ)? {
                    TypeIdentity::Slice(element) => (*element).clone(),
                    _ => {
                        let (module, node) = registry
                            .node(&typ)?
                            .filter(|(_, node)| matches!(node.kind, wire::Array | wire::Slice))
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "invalid_operand",
                                    path,
                                    "construction requires a sequence",
                                )
                            })?;
                        registry.resolve(module, &node.elem)?
                    }
                };
                inputs = Some(vec![element; operands.inputs.len()]);
                outputs = Some(vec![typ]);
            }
            MakeMap(payload) => {
                let typ = resolve(&payload.r#type)?;
                let (module, node) = registry
                    .node(&typ)?
                    .filter(|(_, node)| node.kind == wire::Map)
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_operand", path, "construction requires a map")
                    })?;
                let key = registry.resolve(module, &node.key)?;
                let value = registry.resolve(module, &node.elem)?;
                let mut arguments = Vec::with_capacity(operands.inputs.len());
                for _ in 0..payload.entry_count {
                    arguments.extend([key.clone(), value.clone()]);
                }
                if payload.has_capacity {
                    integer_inputs.push(arguments.len());
                    arguments.push(input(arguments.len()).clone());
                }
                inputs = Some(arguments);
                outputs = Some(vec![typ]);
            }
            MakeSlice(payload) => {
                outputs = Some(vec![resolve(&payload.r#type)?]);
                integer_inputs.push(0);
                if payload.has_capacity {
                    integer_inputs.push(1);
                }
            }
            MakeWaitable(payload) => {
                outputs = Some(vec![resolve(&payload.r#type)?]);
                integer_inputs.push(0);
            }
            Return(_) => inputs = Some(function.result_types.clone()),
            LoadIndirect | StoreIndirect => {
                let element = registry.pointer_element(input(0))?.ok_or_else(|| {
                    RuntimeError::new("invalid_operand", path, "indirect operand is not a pointer")
                })?;
                if matches!(instruction, LoadIndirect) {
                    outputs = Some(vec![element]);
                } else {
                    inputs = Some(vec![input(0).clone(), element]);
                }
            }
            LoadIndex | LoadIndexOk | StoreIndex => {
                let mut typ = input(0).clone();
                if let Some(element) = registry.pointer_element(&typ)? {
                    typ = element;
                }
                let mut map = false;
                let element = match registry.underlying(&typ)? {
                    TypeIdentity::Slice(element) => (*element).clone(),
                    TypeIdentity::Primitive(wire::PrimitiveString) => {
                        TypeIdentity::Primitive(wire::PrimitiveUint8)
                    }
                    _ => {
                        let (module, node) = registry
                            .node(&typ)?
                            .filter(|(_, node)| {
                                matches!(node.kind, wire::Array | wire::Slice | wire::Map)
                            })
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "invalid_operand",
                                    path,
                                    "operand is not indexable",
                                )
                            })?;
                        if node.kind == wire::Map {
                            map = true;
                            inputs =
                                Some(vec![input(0).clone(), registry.resolve(module, &node.key)?]);
                        }
                        registry.resolve(module, &node.elem)?
                    }
                };
                if !map {
                    integer_inputs.push(1);
                    inputs = Some(vec![input(0).clone(), input(1).clone()]);
                }
                if matches!(instruction, StoreIndex) {
                    if registry.underlying(&typ)? == TypeIdentity::Primitive(wire::PrimitiveString)
                    {
                        return Err(RuntimeError::new(
                            "invalid_operand",
                            path,
                            "string index is not writable",
                        ));
                    }
                    inputs.as_mut().unwrap().push(element);
                } else {
                    outputs = Some(vec![element]);
                }
                if matches!(instruction, LoadIndexOk) {
                    if !map {
                        return Err(RuntimeError::new(
                            "invalid_operand",
                            path,
                            "comma-ok index requires map",
                        ));
                    }
                    outputs.as_mut().unwrap().push(boolean.clone());
                }
            }
            LoadField(payload) | StoreField(payload) => {
                let typ = registry
                    .pointer_element(input(0))?
                    .unwrap_or_else(|| input(0).clone());
                let (module, node) = registry
                    .node(&typ)?
                    .filter(|(_, node)| node.kind == wire::Struct)
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_operand", path, "field access requires a struct")
                    })?;
                let field = node
                    .fields
                    .iter()
                    .find(|field| field.name == payload.field)
                    .ok_or_else(|| {
                        RuntimeError::new(
                            "invalid_operand",
                            path,
                            "field is not declared by operand type",
                        )
                    })?;
                let field_type = registry.resolve(module, &field.r#type)?;
                if matches!(instruction, LoadField(_)) {
                    outputs = Some(vec![field_type]);
                } else {
                    inputs = Some(vec![input(0).clone(), field_type]);
                }
            }
            CallValue(_) => {
                let (module, node) = registry
                    .node(input(0))?
                    .filter(|(_, node)| node.kind == wire::Function)
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_operand", path, "callee is not a function")
                    })?;
                let signature = node.signature.as_ref().ok_or_else(|| {
                    RuntimeError::new("invalid_operand", path, "callee has no signature")
                })?;
                let mut arguments = vec![input(0).clone()];
                for parameter in signature.params.iter() {
                    arguments.push(registry.resolve(module, &parameter.r#type)?);
                }
                inputs = Some(arguments);
                outputs = Some(
                    signature
                        .results
                        .iter()
                        .map(|typ| registry.resolve(module, typ))
                        .collect::<Result<_, _>>()?,
                );
            }
            WaitableSend | WaitableTrySend | WaitableRecv | WaitableRecvOk | WaitableTryRecv
            | WaitableCanRecv | WaitableCanSend | WaitableClose => {
                let (module, node) = registry
                    .node(input(0))?
                    .filter(|(_, node)| node.kind == wire::Waitable)
                    .ok_or_else(|| {
                        RuntimeError::new(
                            "invalid_operand",
                            path,
                            "channel operation requires a channel",
                        )
                    })?;
                let sending = matches!(
                    instruction,
                    WaitableSend | WaitableTrySend | WaitableCanSend | WaitableClose
                );
                if sending && node.direction == wire::ChannelReceive
                    || !sending && node.direction == wire::ChannelSend
                {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        "channel direction does not permit operation",
                    ));
                }
                let element = registry.resolve(module, &node.elem)?;
                match instruction {
                    WaitableSend | WaitableTrySend => {
                        inputs = Some(vec![input(0).clone(), element])
                    }
                    WaitableRecv | WaitableRecvOk | WaitableTryRecv => {
                        outputs = Some(vec![element])
                    }
                    _ => {}
                }
                if matches!(
                    instruction,
                    WaitableTrySend
                        | WaitableRecvOk
                        | WaitableTryRecv
                        | WaitableCanRecv
                        | WaitableCanSend
                ) {
                    outputs.get_or_insert_with(Vec::new).push(boolean.clone());
                }
            }
            StringRuneAt | StringNextRuneIndex => {
                if registry.underlying(input(0))? != TypeIdentity::Primitive(wire::PrimitiveString)
                {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        "rune operation requires a string",
                    ));
                }
                integer_inputs.push(1);
                outputs = Some(vec![TypeIdentity::Primitive(
                    if matches!(instruction, StringRuneAt) {
                        wire::PrimitiveInt32
                    } else {
                        wire::PrimitiveInt
                    },
                )]);
            }
            Delete => {
                let (module, node) = registry
                    .node(input(0))?
                    .filter(|(_, node)| node.kind == wire::Map)
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_operand", path, "delete requires a map")
                    })?;
                inputs = Some(vec![input(0).clone(), registry.resolve(module, &node.key)?]);
            }
            Clear => {
                if !matches!(registry.underlying(input(0))?, TypeIdentity::Slice(_))
                    && !registry
                        .node(input(0))?
                        .is_some_and(|(_, node)| matches!(node.kind, wire::Map | wire::Slice))
                {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        "clear requires a map or slice",
                    ));
                }
            }
            JumpIf(_) => {
                if registry.underlying(input(0))? != boolean {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        format!("branch condition is not boolean at {pc}"),
                    ));
                }
            }
            CompareBranch(payload) => {
                if !registry.identical(input(0), &resolve(&payload.r#type)?)? {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        format!("comparison type mismatch at {pc}"),
                    ));
                }
                scalar_result_type(registry, &payload.operator, &[input(0), input(1)], path)?;
            }
            Unary(payload) => {
                outputs = Some(vec![scalar_result_type(
                    registry,
                    &payload.operator,
                    &[input(0)],
                    path,
                )?])
            }
            Binary(payload) => {
                outputs = Some(vec![scalar_result_type(
                    registry,
                    &payload.operator,
                    &[input(0), input(1)],
                    path,
                )?])
            }
            GetPath(payload) => {
                let mut typ = resolve(&payload.r#type)?;
                inputs = Some(vec![typ.clone()]);
                for &index in payload.fields.iter() {
                    if let Some(element) = registry.pointer_element(&typ)? {
                        typ = element;
                    }
                    let (module, node) = registry.node(&typ)?.unwrap();
                    typ = registry.resolve(module, &node.fields[index as usize].r#type)?;
                }
                outputs = Some(vec![typ]);
            }
            CallDirect(call) | TailCallDirect(call) => {
                let module = if call.module_path.is_empty() {
                    &*function.module
                } else {
                    &call.module_path
                };
                let signature = signatures
                    .get(&(module, call.function.as_str()))
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_call", path, "call target does not exist")
                    })?;
                inputs = Some(
                    signature
                        .params
                        .iter()
                        .map(|param| registry.resolve(module, &param.r#type))
                        .collect::<Result<_, _>>()?,
                );
                if matches!(instruction, CallDirect(_)) {
                    outputs = Some(
                        signature
                            .results
                            .iter()
                            .map(|typ| registry.resolve(module, typ))
                            .collect::<Result<_, _>>()?,
                    );
                }
            }
            _ => {}
        }
        for index in integer_inputs {
            if !matches!(registry.underlying(input(index))?, TypeIdentity::Primitive(kind) if (wire::PrimitiveInt..=wire::PrimitiveUintptr).contains(&kind))
            {
                return Err(RuntimeError::new(
                    "invalid_operand",
                    path,
                    format!("input {index}: index or size is not an integer at {pc}"),
                ));
            }
        }
        if let Some(expected) = inputs {
            if expected.len() != operands.inputs.len() {
                return Err(RuntimeError::new(
                    "invalid_operand",
                    path,
                    format!("input signature mismatch at {pc}"),
                ));
            }
            for (index, target) in expected.iter().enumerate() {
                let source = input(index);
                if !assignable(registry, source, target)? {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        format!("input {index} type mismatch at {pc}: {source:?} -> {target:?}"),
                    ));
                }
            }
        }
        if let Some(expected) = outputs {
            if expected.len() != operands.outputs.len() {
                return Err(RuntimeError::new(
                    "invalid_operand",
                    path,
                    format!("output signature mismatch at {pc}"),
                ));
            }
            for (index, typ) in expected.iter().enumerate() {
                let output = operands.outputs[index];
                let actual = if output & LOCAL_OUTPUT != 0 {
                    &function.local_types[(output & !LOCAL_OUTPUT) as usize]
                } else {
                    &function.slot_types[output as usize]
                };
                if !registry.identical(typ, actual)? {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        format!("output {index} type mismatch at {pc}"),
                    ));
                }
            }
        }
    }
    Ok(())
}

fn assignable(
    registry: &crate::types::TypeRegistry,
    source: &TypeIdentity,
    target: &TypeIdentity,
) -> Result<bool, RuntimeError> {
    Ok(registry.identical(source, target)?
        || *source == TypeIdentity::Void && registry.nil_assignable(target)?
        || registry.is_interface(target)? && registry.implements(source, target)?
        || registry.channel_assignable(source, target)?
        || (!matches!(source, TypeIdentity::Named(_)) || !matches!(target, TypeIdentity::Named(_)))
            && registry.identical(&registry.underlying(source)?, &registry.underlying(target)?)?)
}

fn scalar_result_type(
    registry: &crate::types::TypeRegistry,
    operator: &str,
    operands: &[&TypeIdentity],
    path: &str,
) -> Result<TypeIdentity, RuntimeError> {
    let left = operands[0];
    let primitive = match registry.underlying(left)? {
        TypeIdentity::Primitive(value) => value,
        _ => wire::PrimitiveInvalid,
    };
    let number = (wire::PrimitiveInt..=wire::PrimitiveComplex128).contains(&primitive);
    let integer = (wire::PrimitiveInt..=wire::PrimitiveUintptr).contains(&primitive);
    let boolean = TypeIdentity::Primitive(wire::PrimitiveBool);
    let mut result = left.clone();
    let valid = if operands.len() == 1 {
        match operator {
            "+" | "-" => number,
            "^" => integer,
            "!" => {
                result = boolean;
                primitive == wire::PrimitiveBool
            }
            "real" | "imag" => {
                result = TypeIdentity::Primitive(if primitive == wire::PrimitiveComplex64 {
                    wire::PrimitiveFloat32
                } else {
                    wire::PrimitiveFloat64
                });
                matches!(
                    primitive,
                    wire::PrimitiveComplex64 | wire::PrimitiveComplex128
                )
            }
            _ => false,
        }
    } else {
        let right = operands[1];
        let right_primitive = match registry.underlying(right)? {
            TypeIdentity::Primitive(value) => value,
            _ => wire::PrimitiveInvalid,
        };
        let matching = registry.identical(left, right)?;
        match operator {
            "==" | "!=" => {
                result = boolean;
                *left == TypeIdentity::Void && registry.nil_assignable(right)?
                    || *right == TypeIdentity::Void && registry.nil_assignable(left)?
                    || matching && registry.nil_assignable(left)?
                    || registry.comparable(left)?
                        && registry.comparable(right)?
                        && (assignable(registry, left, right)?
                            || assignable(registry, right, left)?)
            }
            "<" | "<=" | ">" | ">=" => {
                result = boolean;
                matching
                    && ((wire::PrimitiveInt..=wire::PrimitiveFloat64).contains(&primitive)
                        || primitive == wire::PrimitiveString)
            }
            "<<" | ">>" => {
                integer && (wire::PrimitiveInt..=wire::PrimitiveUintptr).contains(&right_primitive)
            }
            "&&" | "||" => matching && primitive == wire::PrimitiveBool,
            "complex" => {
                result = TypeIdentity::Primitive(if primitive == wire::PrimitiveFloat32 {
                    wire::PrimitiveComplex64
                } else {
                    wire::PrimitiveComplex128
                });
                matching && matches!(primitive, wire::PrimitiveFloat32 | wire::PrimitiveFloat64)
            }
            "+" => matching && (number || primitive == wire::PrimitiveString),
            "-" | "*" | "/" => matching && number,
            "%" | "&" | "|" | "^" | "&^" => matching && integer,
            _ => false,
        }
    };
    if !valid {
        return Err(RuntimeError::new(
            "invalid_operand",
            path,
            format!("operator {operator} has incompatible operand types"),
        ));
    }
    Ok(result)
}

pub(super) fn verify(
    function: &PreparedFunction,
    labels: &std::collections::HashMap<String, usize>,
    path: &str,
) -> Result<(), RuntimeError> {
    let operands = &function.operands;
    for (pc, instruction) in function.code.iter().enumerate() {
        let (inputs, outputs) = instruction_arity(instruction, path)?;
        if operands.at(pc).inputs.len() != inputs || operands.at(pc).outputs.len() != outputs {
            return Err(RuntimeError::new(
                "invalid_operand",
                path,
                format!("operand arity mismatch at {pc}"),
            ));
        }
    }
    if function.code.is_empty() {
        return Ok(());
    }
    let words = function.slot_types.len().div_ceil(64);
    if words.saturating_mul(function.code.len()) > 4 << 20 {
        return Err(RuntimeError::new(
            "load_limit",
            path,
            "slot initialization analysis exceeds limit",
        ));
    }
    let mut states = vec![None; function.code.len()];
    let mut leaders = vec![false; function.code.len()];
    for &target in labels.values() {
        if target < leaders.len() {
            leaders[target] = true;
        }
    }
    states[0] = Some(vec![0u64; words]);
    let mut queued = vec![false; function.code.len()];
    queued[0] = true;
    let mut queue = vec![0];
    let mut state = vec![0; words];
    let mut successors = Vec::new();
    while let Some(mut pc) = queue.pop() {
        queued[pc] = false;
        state.copy_from_slice(states[pc].as_ref().unwrap());
        // Retain states at control-flow entries, and reuse one scratch bitset
        // while validating each uninterrupted linear path.
        loop {
            let data = operands.at(pc);
            for &slot in data.release_before.iter() {
                state[slot as usize / 64] &= !(1 << (slot % 64));
            }
            for input in data.inputs.iter().filter(|input| input.kind == 0) {
                let slot = input.index as usize;
                if state[slot / 64] & (1 << (slot % 64)) == 0 {
                    return Err(RuntimeError::new(
                        "invalid_operand",
                        path,
                        format!("slot {slot} may be uninitialized at {pc}"),
                    ));
                }
            }
            for &output in data.outputs.iter() {
                if output & LOCAL_OUTPUT != 0 {
                    continue;
                }
                state[output as usize / 64] |= 1 << (output % 64);
            }
            for &slot in data.release.iter() {
                state[slot as usize / 64] &= !(1 << (slot % 64));
            }
            if matches!(
                function.code[pc],
                Instruction::CompareBranch(_)
                    | Instruction::Jump(_)
                    | Instruction::JumpIf(_)
                    | Instruction::TypeDispatch(_)
                    | Instruction::Return(_)
                    | Instruction::Panic
                    | Instruction::TailCallDirect(_)
            ) || pc + 1 == function.code.len()
                || leaders[pc + 1]
            {
                break;
            }
            pc += 1;
        }
        successors.clear();
        match &function.code[pc] {
            Instruction::CompareBranch(payload) => {
                successors.push(labels[&payload.label]);
                successors.push(pc + 1);
            }
            Instruction::Jump(jump) => successors.push(labels[&jump.label]),
            Instruction::JumpIf(jump) => {
                successors.push(labels[&jump.label]);
                successors.push(pc + 1);
            }
            Instruction::TypeDispatch(dispatch) => {
                successors.push(labels[&dispatch.default]);
                successors.extend(dispatch.cases.iter().map(|case| labels[&case.label]));
            }
            Instruction::Return(_) | Instruction::Panic | Instruction::TailCallDirect(_) => {}
            _ => successors.push(pc + 1),
        }
        for &target in &successors {
            if target == function.code.len() {
                continue;
            }
            let mut changed = false;
            match &mut states[target] {
                None => {
                    states[target] = Some(state.clone());
                    changed = true;
                }
                Some(previous) => {
                    for (previous, &next) in previous.iter_mut().zip(&state) {
                        let shared = *previous & next;
                        changed |= shared != *previous;
                        *previous = shared;
                    }
                }
            }
            if changed && !queued[target] {
                queued[target] = true;
                queue.push(target);
            }
        }
    }
    Ok(())
}
