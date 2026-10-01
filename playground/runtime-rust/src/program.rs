//! Immutable execution plans shared by instances. Preparation validates
//! control flow and operands, then releases redundant decoded bodies.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    loader::{DecodedImage, LoadLimits},
};
use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;

#[derive(Debug)]
pub(crate) struct Revision {
    pub generation: u64,
    pub program: Arc<Program>,
    pub logical_functions: Vec<usize>,
}

mod constant;
pub(crate) use constant::PreparedConstant;
mod construct;
pub(crate) use construct::AggregateLayout;
pub(crate) use construct::StructLayout;
pub(crate) use construct::ValueLayout;
mod slots;
use slots::PreparedOperands;

#[derive(Clone)]
pub(crate) struct PreparedField {
    pub index: usize,
    pub indirect: bool,
}

pub(crate) use wire::Instruction;

pub(crate) const LOCAL_OUTPUT: u32 = 1 << 31;

// Only operations still dispatched through their wire descriptor retain one
// after preparation. Typed operators, locals, branches and intrinsics execute
// exclusively from PreparedInstruction. PCs remain stable for symbols/retry.
#[derive(Clone, Default)]
pub(crate) struct PreparedCode {
    indices: Vec<u32>,
    descriptors: Vec<Instruction>,
}

impl PreparedCode {
    fn push(&mut self, instruction: Instruction) {
        self.indices.push(self.descriptors.len() as u32);
        self.descriptors.push(instruction);
    }

    pub(crate) fn len(&self) -> usize {
        self.indices.len()
    }
    pub(crate) fn is_empty(&self) -> bool {
        self.indices.is_empty()
    }

    pub(crate) fn descriptor(&self, pc: usize) -> Option<&Instruction> {
        self.descriptors.get(self.indices[pc] as usize)
    }

    // Used only by loader validation, before compacting prepared operations.
    fn iter(&self) -> impl Iterator<Item = &Instruction> {
        self.descriptors.iter()
    }

    fn compact(&mut self, execution: &[PreparedInstruction]) {
        let previous = std::mem::take(&mut self.descriptors);
        self.descriptors = Vec::with_capacity(
            execution
                .iter()
                .filter(|instruction| {
                    matches!(
                        instruction,
                        PreparedInstruction::Operand | PreparedInstruction::Construct(_)
                    )
                })
                .count(),
        );
        for (pc, instruction) in previous.into_iter().enumerate() {
            self.indices[pc] = if matches!(
                execution[pc],
                PreparedInstruction::Operand | PreparedInstruction::Construct(_)
            ) {
                let index = self.descriptors.len() as u32;
                self.descriptors.push(instruction);
                index
            } else {
                u32::MAX
            };
        }
    }
}

impl std::ops::Index<usize> for PreparedCode {
    type Output = Instruction;
    fn index(&self, pc: usize) -> &Instruction {
        self.descriptor(pc)
            .expect("operation requires a wire descriptor")
    }
}

#[derive(Clone)]
pub(crate) struct PreparedFunction {
    pub index: usize,
    pub module_index: usize,
    pub module: Arc<str>,
    pub name: Arc<str>,
    pub local_types: Vec<crate::types::TypeIdentity>,
    pub addressable_locals: Vec<bool>,
    pub result_types: Vec<crate::types::TypeIdentity>,
    pub result_locals: Vec<usize>,
    pub execution: Vec<PreparedInstruction>,
    pub operand_types: Vec<Option<crate::types::TypeIdentity>>,
    pub type_dispatches: Vec<PreparedTypeDispatch>,
    pub aggregates: Vec<AggregateLayout>,
    pub field_paths: Vec<Vec<PreparedField>>,
    pub globals: Vec<String>,
    pub targets: Vec<Option<usize>>,
    pub locations: Vec<Vec<wire::Location>>,
    pub declaration: wire::Function,
    pub code: PreparedCode,
    pub opcodes: Vec<&'static str>,
    pub locals: HashMap<String, usize>,
    pub upvalues: HashMap<String, usize>,
    pub slot_types: Vec<crate::types::TypeIdentity>,
    pub operands: PreparedOperands,
    // Only direct locals and non-scalar constants may need owner-side loading.
    pub lazy_slot_inputs: Vec<bool>,
}

#[derive(Clone, Copy)]
pub(crate) enum PreparedInstruction {
    GetPath(usize),
    CompareBranch {
        operator: crate::operators::PreparedOperator,
        target: usize,
        when: bool,
    },
    Construct(usize),
    TypeDispatch(usize),
    Intrinsic(wire::Intrinsic),
    Unary(crate::operators::PreparedOperator),
    Binary(crate::operators::PreparedOperator),
    Constant(usize),
    Local {
        index: usize,
        // A single temporary released on every successor can be transferred
        // after the private store's checks. u32::MAX means borrow/copy.
        move_source: u32,
        store: bool,
        rebind: bool,
    },
    Upvalue {
        index: usize,
        store: bool,
    },
    Global {
        index: usize,
        store: bool,
    },
    Jump {
        target: usize,
        conditional: bool,
        negate: bool,
    },
    Operand,
}

#[derive(Clone)]
pub(crate) struct PreparedTypeDispatch {
    pub subject: usize,
    pub fallback: usize,
    pub fallback_local: Option<usize>,
    pub cases: Vec<PreparedTypeCase>,
    pub concrete: HashMap<Option<crate::types::TypeIdentity>, usize>,
    pub concrete_end: usize,
}

pub(crate) fn concrete_dispatch_identity(
    types: &crate::types::TypeRegistry,
    typ: &crate::types::TypeIdentity,
) -> Result<bool, RuntimeError> {
    use crate::types::TypeIdentity;
    let (leaf, pointer) = match typ {
        TypeIdentity::Pointer(element) => (element.as_ref(), true),
        _ => (typ, false),
    };
    Ok(match leaf {
        TypeIdentity::Primitive(primitive) => {
            (wire::PrimitiveBool..=wire::PrimitiveComplex128).contains(primitive)
        }
        TypeIdentity::Named(_) => {
            pointer
                || (types.underlying(leaf)? != TypeIdentity::Any
                    && types.underlying(leaf)? != TypeIdentity::Primitive(wire::PrimitiveError)
                    && !types.node(leaf)?.is_some_and(|(_, node)| {
                        matches!(node.kind, wire::Interface | wire::Function)
                    }))
        }
        _ => false,
    })
}

#[derive(Clone)]
pub(crate) struct PreparedTypeCase {
    pub typ: Option<crate::types::TypeIdentity>,
    pub target: usize,
    pub binding: Option<usize>,
    pub original: bool,
}

pub struct Program {
    pub(crate) symbols: Option<wire::ProgramSymbols>,
    pub(crate) decoded: DecodedImage,
    pub(crate) functions: BTreeMap<(String, String), usize>,
    function_lookup: HashMap<String, HashMap<String, usize>>,
    pub(crate) function_table: Vec<Arc<PreparedFunction>>,
    pub(crate) constants: BTreeMap<(String, String), usize>,
    pub(crate) constant_data: Vec<PreparedConstant>,
}

impl std::fmt::Debug for Program {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("Program")
            .field("hash", &self.image().hash)
            .finish_non_exhaustive()
    }
}

impl Program {
    pub fn types(&self) -> &crate::types::TypeRegistry {
        self.decoded.types()
    }
    #[cfg(not(target_arch = "wasm32"))]
    pub fn instantiate(
        self: &Arc<Self>,
        options: crate::execution::InstanceOptions,
    ) -> Result<crate::execution::SharedInstance, RuntimeError> {
        if options.cancellation.is_cancelled() {
            return Err(RuntimeError::new(
                "canceled",
                "instance",
                "initialization canceled",
            ));
        }
        let parallelism = options.parallelism.max(1);
        let executor = options.executor.clone();
        #[cfg(test)]
        let task_observer = options.task_observer.clone();
        #[cfg(not(test))]
        let task_observer = None;
        let mut machine = match options.bridge {
            Some(bridge) => crate::instance::Instance::with_bridge(
                self.clone(),
                options.limits,
                bridge.as_ref(),
            )?,
            None => crate::instance::Instance::new(self.clone(), options.limits)?,
        };
        machine.set_environment(options.clock, options.entropy)?;
        machine.initialize_root(&options.cancellation)?;
        crate::execution::SharedInstance::from_machine_with_options(
            machine,
            parallelism,
            executor,
            task_observer,
        )
    }
    pub fn load(bytes: &[u8], limits: LoadLimits) -> Result<Self, RuntimeError> {
        Self::prepare(DecodedImage::decode(bytes, limits)?)
    }

    pub fn prepare(decoded: DecodedImage) -> Result<Self, RuntimeError> {
        let manifest: serde_json::Value = serde_json::from_str(wire::CONTRACT_JSON)?;
        let intrinsic_contracts: HashMap<_, _> = manifest["intrinsics"]
            .as_array()
            .unwrap()
            .iter()
            .map(|descriptor| {
                (
                    descriptor["ID"].as_str().unwrap(),
                    (
                        descriptor["ArgCount"].as_i64().unwrap(),
                        descriptor["ResultCount"].as_i64().unwrap(),
                    ),
                )
            })
            .collect();
        let mut functions = BTreeMap::new();
        let mut prepared_constants = BTreeMap::new();
        let mut constant_data = Vec::new();
        let mut constant_types = Vec::new();
        let signatures: BTreeMap<(&str, &str), &wire::FunctionSignature> = decoded
            .artifacts()
            .iter()
            .flat_map(|(module, artifact)| {
                artifact.functions.iter().map(move |function| {
                    ((module.as_str(), function.id.as_str()), &function.signature)
                })
            })
            .collect();
        for (module_index, (module, artifact)) in decoded.artifacts().iter().enumerate() {
            let constants = unique_ids(artifact.constants.iter().map(|value| &value.id), module)?;
            let globals = unique_ids(artifact.globals.iter().map(|value| &value.id), module)?;
            unique_ids(artifact.functions.iter().map(|value| &value.id), module)?;
            unique_ids(artifact.exports.iter().map(|value| &value.name), module)?;
            for constant in artifact.constants.iter() {
                prepared_constants
                    .insert((module.clone(), constant.id.clone()), constant_data.len());
                constant_data.push(PreparedConstant::decode(decoded.types(), module, constant)?);
                constant_types.push(decoded.types().resolve(module, &constant.r#type)?);
            }
            for declaration in artifact.functions.iter() {
                let path = format!("{module}/{}", declaration.id);
                let code = declaration
                    .code
                    .as_ref()
                    .ok_or_else(|| RuntimeError::new("invalid_code", &path, "missing slot code"))?;
                let locals = unique_ids(declaration.locals.iter().map(|value| &value.id), &path)?;
                let upvalues =
                    unique_ids(declaration.upvalues.iter().map(|value| &value.id), &path)?;
                for local in declaration.locals.iter() {
                    decoded.types().resolve(module, &local.r#type)?;
                }
                for upvalue in declaration.upvalues.iter() {
                    decoded.types().resolve(module, &upvalue.r#type)?;
                }
                if declaration.signature.params.len() > locals.len() {
                    return Err(RuntimeError::new(
                        "invalid_function",
                        &path,
                        "missing parameter locals",
                    ));
                }
                let mut labels = HashMap::new();
                let mut function = PreparedFunction {
                    addressable_locals: vec![false; declaration.locals.len()],
                    targets: Vec::new(),
                    execution: Vec::new(),
                    operand_types: Vec::new(),
                    type_dispatches: Vec::new(),
                    aggregates: Vec::new(),
                    field_paths: Vec::new(),
                    globals: artifact
                        .globals
                        .iter()
                        .map(|global| global.id.clone())
                        .collect(),
                    index: functions.len(),
                    module_index,
                    module: module.as_str().into(),
                    name: declaration.id.as_str().into(),
                    local_types: declaration
                        .locals
                        .iter()
                        .map(|local| decoded.types().resolve(module, &local.r#type))
                        .collect::<Result<_, _>>()?,
                    result_locals: Vec::new(),
                    result_types: declaration
                        .signature
                        .results
                        .iter()
                        .map(|typ| decoded.types().resolve(module, typ))
                        .collect::<Result<_, _>>()?,
                    locations: Vec::new(),
                    declaration: declaration.clone(),
                    code: PreparedCode::default(),
                    opcodes: Vec::new(),
                    locals,
                    upvalues,
                    slot_types: code
                        .types
                        .iter()
                        .map(|typ| decoded.types().resolve(module, typ))
                        .collect::<Result<Vec<_>, _>>()?,
                    operands: PreparedOperands::default(),
                    lazy_slot_inputs: Vec::new(),
                };
                if !declaration.result_locals.is_empty() {
                    let results = unique_ids(declaration.result_locals.iter(), &path)?;
                    if results.len() != declaration.signature.results.len()
                        || results.keys().any(|id| !function.locals.contains_key(id))
                    {
                        return Err(RuntimeError::new(
                            "invalid_result_local",
                            &path,
                            "result locals do not match signature or local slots",
                        ));
                    }
                    function.result_locals = declaration
                        .result_locals
                        .iter()
                        .map(|id| function.locals[id])
                        .collect();
                }
                let operations = {
                    code.instructions
                        .iter()
                        .map(|instruction| {
                            code.descriptors
                                .instruction(instruction.op, instruction.descriptor)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "invalid_descriptor",
                                        &path,
                                        "unknown operation or descriptor",
                                    )
                                })
                        })
                        .collect::<Result<Vec<_>, _>>()?
                };
                for (position, raw) in operations.iter().enumerate() {
                    let instruction = raw.clone();
                    if let Instruction::Label(label) = instruction {
                        {
                            let operands = code
                                .operands
                                .get(code.instructions[position].operands as usize)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "invalid_operand",
                                        &path,
                                        "label operand descriptor out of range",
                                    )
                                })?;
                            if !operands.inputs.is_empty()
                                || !operands.outputs.is_empty()
                                || !operands.release.is_empty()
                                || !operands.release_before.is_empty()
                            {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "label cannot carry operands or releases",
                                ));
                            }
                        }
                        if label.label.is_empty()
                            || labels.insert(label.label, function.code.len()).is_some()
                        {
                            return Err(RuntimeError::new(
                                "invalid_label",
                                &path,
                                "empty or duplicate label",
                            ));
                        }
                        continue;
                    }
                    {
                        let record = &code.instructions[position];
                        let mut operands = code
                            .operands
                            .get(record.operands as usize)
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "operand descriptor out of range",
                                )
                            })?
                            .clone();
                        for input in operands.inputs.iter_mut() {
                            match input.kind {
                                0 if (input.index as usize) < function.slot_types.len() => {}
                                2 if (input.index as usize) < function.local_types.len() => {}
                                1 => {
                                    let constant = artifact
                                        .constants
                                        .get(input.index as usize)
                                        .ok_or_else(|| {
                                            RuntimeError::new(
                                                "invalid_operand",
                                                &path,
                                                "constant index out of range",
                                            )
                                        })?;
                                    if constant.untyped {
                                        return Err(RuntimeError::new(
                                            "invalid_operand",
                                            &path,
                                            "untyped constant is not an execution operand",
                                        ));
                                    }
                                    input.index = u32::try_from(
                                        prepared_constants[&(module.clone(), constant.id.clone())],
                                    )
                                    .map_err(|_| {
                                        RuntimeError::new(
                                            "load_limit",
                                            &path,
                                            "constant index exceeds u32",
                                        )
                                    })?;
                                }
                                _ => {
                                    return Err(RuntimeError::new(
                                        "invalid_operand",
                                        &path,
                                        "input slot or kind out of range",
                                    ));
                                }
                            }
                        }
                        for &output in operands.outputs.iter() {
                            let valid = if output & LOCAL_OUTPUT != 0 {
                                matches!(
                                    instruction,
                                    Instruction::Unary(_)
                                        | Instruction::Binary(_)
                                        | Instruction::Zero(_)
                                        | Instruction::Len
                                        | Instruction::Cap
                                        | Instruction::GetPath(_)
                                        | Instruction::Convert(_)
                                        | Instruction::LoadIndex
                                ) && operands.outputs.len() == 1
                                    && ((output & !LOCAL_OUTPUT) as usize)
                                        < function.local_types.len()
                            } else {
                                (output as usize) < function.slot_types.len()
                            };
                            if !valid {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "invalid output destination",
                                ));
                            }
                        }
                        for slot in operands
                            .release
                            .iter()
                            .chain(operands.release_before.iter())
                        {
                            if *slot as usize >= function.slot_types.len() {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "output or release slot out of range",
                                ));
                            }
                        }
                        for slots in [
                            &operands.outputs,
                            &operands.release,
                            &operands.release_before,
                        ] {
                            if slots.len() <= 1 {
                                continue;
                            }
                            let mut seen = std::collections::HashSet::with_capacity(slots.len());
                            if slots.iter().any(|slot| !seen.insert(*slot)) {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "duplicate output or release slot",
                                ));
                            }
                        }
                        if operands.outputs.iter().any(|output| {
                            operands
                                .inputs
                                .iter()
                                .any(|input| input.kind == 0 && input.index == *output)
                        }) {
                            return Err(RuntimeError::new(
                                "invalid_operand",
                                &path,
                                "output aliases an input slot",
                            ));
                        }
                        function
                            .lazy_slot_inputs
                            .push(operands.inputs.iter().any(|input| {
                                input.kind == 2
                                    || input.kind == 1
                                        && !matches!(
                                            constant_data[input.index as usize],
                                            PreparedConstant::Scalar(_)
                                        )
                            }));
                        function.operands.push(operands);
                    }
                    let valid = match &instruction {
                        Instruction::CompareBranch(payload) => {
                            matches!(
                                payload.operator.as_str(),
                                "==" | "!=" | "<" | "<=" | ">" | ">="
                            ) && decoded.types().resolve(module, &payload.r#type).is_ok()
                        }
                        Instruction::TypeDispatch(payload) => {
                            function.locals.contains_key(&payload.subject)
                                && (payload.default_local.is_empty()
                                    || function.locals.contains_key(&payload.default_local))
                                && payload.cases.iter().all(|case| {
                                    (case.binding.is_empty()
                                        || function.locals.contains_key(&case.binding))
                                        && (case.r#type.kind != 0
                                            || (case.r#type == wire::TypeRef::default()
                                                && case.original))
                                })
                        }
                        Instruction::Select(payload) => {
                            function.locals.contains_key(&payload.index)
                                && payload.cases.iter().all(|case| {
                                    function.locals.contains_key(&case.channel)
                                        && if case.send.is_empty() {
                                            function.locals.contains_key(&case.value)
                                                && function.locals.contains_key(&case.ok)
                                        } else {
                                            case.value.is_empty()
                                                && case.ok.is_empty()
                                                && function.locals.contains_key(&case.send)
                                        }
                                })
                        }
                        Instruction::Const(value) => constants
                            .get(&value.constant)
                            .is_some_and(|index| !artifact.constants[*index].untyped),
                        Instruction::LoadLocal(value)
                        | Instruction::StoreLocal(value)
                        | Instruction::MapIterInit(value)
                        | Instruction::MapIterNext(value)
                        | Instruction::MapIterClose(value) => {
                            function.locals.contains_key(&value.local)
                        }
                        Instruction::LoadUpvalue(value) | Instruction::StoreUpvalue(value) => {
                            function.upvalues.contains_key(&value.upvalue)
                        }
                        Instruction::LoadGlobal(value) | Instruction::StoreGlobal(value) => {
                            globals.contains_key(&value.global)
                        }
                        Instruction::Zero(value)
                        | Instruction::Convert(value)
                        | Instruction::TypeAssert(value)
                        | Instruction::TypeAssertOk(value) => {
                            decoded.types().resolve(module, &value.r#type)?;
                            true
                        }
                        Instruction::MakeStruct(value) => {
                            decoded.types().resolve(module, &value.r#type)?;
                            true
                        }
                        Instruction::MakeSequence(value) => {
                            decoded.types().resolve(module, &value.r#type)?;
                            value.element_count >= 0
                        }
                        Instruction::MakeSlice(value) => {
                            decoded.types().resolve(module, &value.r#type)?;
                            true
                        }
                        Instruction::MakeMap(value) => {
                            decoded.types().resolve(module, &value.r#type)?;
                            value.entry_count >= 0 && value.entry_count.checked_mul(2).is_some()
                        }
                        Instruction::Append(value) => {
                            value.count >= 0 && (!value.expand || value.count == 1)
                        }
                        Instruction::AddressOf(value) => valid_address(value, &function, &globals),
                        Instruction::MakeClosure(value) => value
                            .captures
                            .iter()
                            .all(|value| valid_address(value, &function, &globals)),
                        Instruction::DeferPush(value) => value.owner_depth >= 0,
                        Instruction::CallFfi(value) => {
                            value.arg_count == 2 && value.result_count == 3
                        }
                        Instruction::CallInterface(value) => {
                            let typ = decoded.types().resolve(module, &value.interface_type)?;
                            value.arg_count >= 0
                                && value.result_count >= 0
                                && decoded
                                    .types()
                                    .node(&typ)?
                                    .is_some_and(|(_, node)| node.kind == wire::Interface)
                                && decoded.types().declared_methods(&typ)?.iter().any(
                                    |(_, method)| {
                                        method.name == value.method
                                            && method.signature.params.len()
                                                == value.arg_count as usize
                                            && method.signature.results.len()
                                                == value.result_count as usize
                                    },
                                )
                        }
                        Instruction::CallIntrinsic(value) => {
                            intrinsic_contracts.get(value.id.as_str())
                                == Some(&(value.arg_count, value.result_count))
                        }
                        _ => true,
                    };
                    if !valid {
                        return Err(RuntimeError::new(
                            "invalid_operand",
                            &path,
                            format!("{:?}", instruction),
                        ));
                    }
                    let addresses: &[wire::AddressPayload] = match &instruction {
                        Instruction::AddressOf(address) => std::slice::from_ref(address),
                        Instruction::MakeClosure(closure) => &closure.captures,
                        _ => &[],
                    };
                    for address in addresses {
                        if address.kind == "local" {
                            function.addressable_locals[function.locals[&address.local]] = true;
                        }
                    }
                    function.code.push(instruction);
                    function.opcodes.push(raw.opcode());
                }
                verify_control_flow(&function, &labels, &path)?;
                for (pc, instruction) in function.code.iter().enumerate() {
                    use Instruction::*;
                    function.execution.push(match instruction {
                        GetPath(payload) => {
                            if payload.fields.is_empty() || payload.fields.len() > 16 {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "field path requires 1 to 16 fields",
                                ));
                            }
                            let mut typ = decoded.types().resolve(module, &payload.r#type)?;
                            let mut steps = Vec::with_capacity(payload.fields.len());
                            for &index in payload.fields.iter() {
                                let element = decoded.types().pointer_element(&typ)?;
                                let indirect = element.is_some();
                                if let Some(element) = element {
                                    typ = element;
                                }
                                let (owner, node) = decoded
                                    .types()
                                    .node(&typ)?
                                    .filter(|(_, node)| node.kind == wire::Struct)
                                    .ok_or_else(|| {
                                        RuntimeError::new(
                                            "invalid_operand",
                                            &path,
                                            "field path requires struct",
                                        )
                                    })?;
                                let field = node
                                    .fields
                                    .get(index as usize)
                                    .filter(|field| field.name != "_")
                                    .ok_or_else(|| {
                                        RuntimeError::new(
                                            "invalid_operand",
                                            &path,
                                            "field path index out of range",
                                        )
                                    })?;
                                let names: std::collections::BTreeSet<_> =
                                    node.fields.iter().map(|field| &field.name).collect();
                                steps.push(PreparedField {
                                    indirect,
                                    index: names
                                        .iter()
                                        .position(|name| **name == field.name)
                                        .unwrap(),
                                });
                                typ = decoded.types().resolve(owner, &field.r#type)?;
                            }
                            let index = function.field_paths.len();
                            function.field_paths.push(steps);
                            PreparedInstruction::GetPath(index)
                        }
                        CompareBranch(payload) => PreparedInstruction::CompareBranch {
                            operator: crate::operators::PreparedOperator::parse(&payload.operator)?,
                            target: labels[&payload.label],
                            when: payload.when,
                        },
                        MakeStruct(payload) => {
                            let index = function.aggregates.len();
                            function.aggregates.push(AggregateLayout::Struct(
                                StructLayout::prepare(
                                    decoded.types(),
                                    &decoded.types().resolve(module, &payload.r#type)?,
                                    &payload.fields,
                                )?,
                            ));
                            PreparedInstruction::Construct(index)
                        }
                        MakeSequence(payload) => {
                            let index = function.aggregates.len();
                            function.aggregates.push(AggregateLayout::Sequence(
                                ValueLayout::prepare(
                                    decoded.types(),
                                    &decoded.types().resolve(module, &payload.r#type)?,
                                )?,
                            ));
                            PreparedInstruction::Construct(index)
                        }
                        TypeDispatch(payload) => {
                            let index = function.type_dispatches.len();
                            let subject = &function.local_types[function.locals[&payload.subject]];
                            let underlying = decoded.types().underlying(subject)?;
                            if underlying != crate::types::TypeIdentity::Any
                                && underlying
                                    != crate::types::TypeIdentity::Primitive(wire::PrimitiveError)
                                && !decoded
                                    .types()
                                    .node(subject)?
                                    .is_some_and(|(_, node)| node.kind == wire::Interface)
                            {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "type dispatch subject must be an interface",
                                ));
                            }
                            if let Some(binding) = function.locals.get(&payload.default_local)
                                && !decoded
                                    .types()
                                    .identical(&function.local_types[*binding], subject)?
                            {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "type dispatch default binding must match subject type",
                                ));
                            }
                            let mut cases = Vec::with_capacity(payload.cases.len());
                            let mut concrete = HashMap::new();
                            let mut concrete_end = 0;
                            for case in payload.cases.iter() {
                                cases.push(PreparedTypeCase {
                                    typ: if case.r#type.kind == 0 {
                                        None
                                    } else {
                                        Some(decoded.types().resolve(module, &case.r#type)?)
                                    },
                                    target: labels[&case.label],
                                    binding: function.locals.get(&case.binding).copied(),
                                    original: case.original,
                                });
                                let entry = cases.last().unwrap();
                                if let Some(binding) = entry.binding {
                                    let expected = if entry.original {
                                        subject
                                    } else {
                                        entry.typ.as_ref().unwrap()
                                    };
                                    if !decoded
                                        .types()
                                        .identical(&function.local_types[binding], expected)?
                                    {
                                        return Err(RuntimeError::new(
                                            "invalid_operand",
                                            &path,
                                            "type dispatch case binding has incompatible type",
                                        ));
                                    }
                                }
                                if concrete_end + 1 == cases.len()
                                    && match &entry.typ {
                                        None => true,
                                        Some(typ) => {
                                            concrete_dispatch_identity(decoded.types(), typ)?
                                        }
                                    }
                                {
                                    concrete.entry(entry.typ.clone()).or_insert(concrete_end);
                                    concrete_end += 1;
                                }
                            }
                            function.type_dispatches.push(PreparedTypeDispatch {
                                subject: function.locals[&payload.subject],
                                fallback: labels[&payload.default],
                                fallback_local: function
                                    .locals
                                    .get(&payload.default_local)
                                    .copied(),
                                cases,
                                concrete,
                                concrete_end,
                            });
                            PreparedInstruction::TypeDispatch(index)
                        }
                        CallIntrinsic(value) => PreparedInstruction::Intrinsic(
                            wire::Intrinsic::from_id(&value.id).unwrap(),
                        ),
                        Unary(value) => PreparedInstruction::Unary(
                            crate::operators::PreparedOperator::parse(&value.operator)?,
                        ),
                        Binary(value) => PreparedInstruction::Binary(
                            crate::operators::PreparedOperator::parse(&value.operator)?,
                        ),
                        Const(value) => PreparedInstruction::Constant(
                            prepared_constants[&(module.clone(), value.constant.clone())],
                        ),
                        LoadLocal(value) | StoreLocal(value) => PreparedInstruction::Local {
                            index: function.locals[&value.local],
                            move_source: {
                                let operands = function.operands.at(pc);
                                match operands.inputs {
                                    [input]
                                        if matches!(instruction, StoreLocal(_))
                                            && input.kind == 0
                                            && operands.release.contains(&input.index) =>
                                    {
                                        input.index
                                    }
                                    _ => u32::MAX,
                                }
                            },
                            store: matches!(instruction, StoreLocal(_)),
                            rebind: value.rebind,
                        },
                        LoadUpvalue(value) | StoreUpvalue(value) => PreparedInstruction::Upvalue {
                            index: function.upvalues[&value.upvalue],
                            store: matches!(instruction, StoreUpvalue(_)),
                        },
                        LoadGlobal(value) | StoreGlobal(value) => PreparedInstruction::Global {
                            index: globals[&value.global],
                            store: matches!(instruction, StoreGlobal(_)),
                        },
                        Jump(value) | JumpIf(value) => PreparedInstruction::Jump {
                            target: labels[&value.label],
                            conditional: matches!(instruction, JumpIf(_)),
                            negate: value.negate,
                        },
                        _ => PreparedInstruction::Operand,
                    });
                    let typ = match instruction {
                        Zero(value) | Convert(value) | TypeAssert(value) | TypeAssertOk(value) => {
                            Some(&value.r#type)
                        }
                        MakeStruct(value) => Some(&value.r#type),
                        MakeSequence(value) => Some(&value.r#type),
                        MakeSlice(value) => Some(&value.r#type),
                        MakeMap(value) => Some(&value.r#type),
                        _ => None,
                    };
                    function.operand_types.push(
                        typ.map(|typ| decoded.types().resolve(module, typ))
                            .transpose()?,
                    );
                }
                {
                    slots::verify_types(&function, &decoded, &constant_types, &signatures, &path)?;
                    for (pc, operands) in function.operands.iter().enumerate() {
                        let operation = match &mut function.execution[pc] {
                            PreparedInstruction::Unary(operation)
                            | PreparedInstruction::Binary(operation)
                            | PreparedInstruction::CompareBranch {
                                operator: operation,
                                ..
                            } => operation,
                            _ => continue,
                        };
                        for (index, input) in operands.inputs.iter().enumerate() {
                            let typ = match input.kind {
                                0 => &function.slot_types[input.index as usize],
                                2 => &function.local_types[input.index as usize],
                                _ => &constant_types[input.index as usize],
                            };
                            operation.bind(index, typ, decoded.types())?;
                        }
                    }
                }
                function.declaration.code = None;
                functions.insert((module.clone(), declaration.id.clone()), Arc::new(function));
            }
        }
        let mut function_table = vec![None; functions.len()];
        let mut function_lookup: HashMap<String, HashMap<String, usize>> = HashMap::new();
        let mut function_indices = BTreeMap::new();
        for ((module, name), function) in functions {
            let index = function.index;
            function_lookup
                .entry(module.clone())
                .or_default()
                .insert(name.clone(), index);
            function_indices.insert((module, name), index);
            function_table[index] = Some(function);
        }
        let mut program = Self {
            symbols: None,
            decoded,
            functions: function_indices,
            function_lookup,
            function_table: function_table.into_iter().map(Option::unwrap).collect(),
            constants: prepared_constants,
            constant_data,
        };
        for ((module, id), index) in &program.functions {
            let function = &program.function_table[*index];
            for instruction in function.code.iter() {
                match instruction {
                    Instruction::CallDirect(call) | Instruction::TailCallDirect(call) => {
                        let target = program.function(
                            if call.module_path.is_empty() {
                                module
                            } else {
                                &call.module_path
                            },
                            &call.function,
                        )?;
                        if call.arg_count != target.declaration.signature.params.len() as i64
                            || call.result_count
                                != target.declaration.signature.results.len() as i64
                            || !target.declaration.upvalues.is_empty()
                        {
                            return Err(RuntimeError::new(
                                "invalid_call",
                                id,
                                "direct call signature mismatch",
                            ));
                        }
                    }
                    Instruction::MakeClosure(closure) => {
                        let target = program.function(
                            if closure.module_path.is_empty() {
                                module
                            } else {
                                &closure.module_path
                            },
                            &closure.function,
                        )?;
                        if closure.captures.len() != target.declaration.upvalues.len() {
                            return Err(RuntimeError::new(
                                "invalid_capture",
                                id,
                                "capture count mismatch",
                            ));
                        }
                    }
                    Instruction::LoadExport(export) => {
                        program.export(&export.module_path, &export.export)?;
                    }
                    Instruction::InitModule(init)
                        if !program.decoded.artifacts().contains_key(&init.module_path) =>
                    {
                        return Err(RuntimeError::new("missing_module", id, &init.module_path));
                    }
                    _ => {}
                }
            }
        }
        for index in 0..program.function_table.len() {
            let function = &program.function_table[index];
            let targets = function
                .code
                .iter()
                .map(|instruction| {
                    let (module, name) = match instruction {
                        Instruction::CallDirect(call) | Instruction::TailCallDirect(call) => {
                            (&call.module_path, &call.function)
                        }
                        Instruction::MakeClosure(closure) => {
                            (&closure.module_path, &closure.function)
                        }
                        _ => return Ok(None),
                    };
                    Ok(Some(
                        program
                            .function(
                                if module.is_empty() {
                                    &function.module
                                } else {
                                    module
                                },
                                name,
                            )?
                            .index,
                    ))
                })
                .collect::<Result<Vec<_>, RuntimeError>>()?;
            let function = Arc::get_mut(&mut program.function_table[index]).unwrap();
            function.targets = targets;
            function.code.compact(&function.execution);
        }
        program.decoded.release_function_bodies();
        Ok(program)
    }

    pub fn image(&self) -> &wire::ExecutionImage {
        self.decoded.image()
    }

    pub fn with_symbols(mut self, symbols: wire::ProgramSymbols) -> Result<Self, RuntimeError> {
        crate::symbols::validate(&self, &symbols)?;
        if let Some(packages) = &symbols.packages {
            for (module, package) in packages {
                for symbol in package.functions.iter() {
                    if let Some(function) =
                        self.functions.get_mut(&(module.clone(), symbol.id.clone()))
                    {
                        let function = Arc::make_mut(&mut self.function_table[*function]);
                        function.locations = vec![Vec::new(); function.code.len()];
                        for location in symbol.locations.iter() {
                            if let Some(points) = function.locations.get_mut(location.pc as usize) {
                                *points = location.points.to_vec();
                            }
                        }
                    }
                }
            }
        }
        self.symbols = Some(symbols);
        Ok(self)
    }

    pub fn symbols(&self) -> Option<&wire::ProgramSymbols> {
        self.symbols.as_ref()
    }

    pub(crate) fn function(
        &self,
        module: &str,
        id: &str,
    ) -> Result<&Arc<PreparedFunction>, RuntimeError> {
        self.function_lookup
            .get(module)
            .and_then(|functions| functions.get(id))
            .map(|index| &self.function_table[*index])
            .ok_or_else(|| RuntimeError::new("missing_function", module, id))
    }

    pub(crate) fn export(&self, module: &str, name: &str) -> Result<&wire::Export, RuntimeError> {
        self.decoded
            .artifacts()
            .get(module)
            .and_then(|artifact| artifact.exports.iter().find(|export| export.name == name))
            .ok_or_else(|| RuntimeError::new("missing_export", module, name))
    }
}

fn unique_ids<'a>(
    ids: impl Iterator<Item = &'a String>,
    path: &str,
) -> Result<HashMap<String, usize>, RuntimeError> {
    let mut index = HashMap::new();
    for (position, id) in ids.enumerate() {
        if id.is_empty() || index.insert(id.clone(), position).is_some() {
            return Err(RuntimeError::new(
                "invalid_id",
                path,
                "empty or duplicate identifier",
            ));
        }
    }
    Ok(index)
}

fn valid_address(
    address: &wire::AddressPayload,
    function: &PreparedFunction,
    globals: &HashMap<String, usize>,
) -> bool {
    let valid = match address.kind.as_str() {
        "local" => function.locals.contains_key(&address.local),
        "upvalue" => function.upvalues.contains_key(&address.upvalue),
        "global" => globals.contains_key(&address.global),
        "export" => !address.module_path.is_empty() && !address.export.is_empty(),
        _ => false,
    };
    valid
        && address
            .path
            .iter()
            .all(|segment| match segment.kind.as_str() {
                "field" => !segment.field.is_empty(),
                "index" => function.locals.contains_key(&segment.local),
                "indirect" => true,
                _ => false,
            })
}

fn verify_control_flow(
    function: &PreparedFunction,
    labels: &HashMap<String, usize>,
    path: &str,
) -> Result<(), RuntimeError> {
    // Validate labels and counts even in unreachable instructions.
    for instruction in function.code.iter() {
        match instruction {
            Instruction::CompareBranch(payload) if !labels.contains_key(&payload.label) => {
                return Err(RuntimeError::new("invalid_jump", path, &payload.label));
            }
            Instruction::TypeDispatch(payload) => {
                if !labels.contains_key(&payload.default)
                    || payload
                        .cases
                        .iter()
                        .any(|case| !labels.contains_key(&case.label))
                {
                    return Err(RuntimeError::new(
                        "invalid_jump",
                        path,
                        "unknown type dispatch label",
                    ));
                }
            }
            Instruction::Jump(jump) | Instruction::JumpIf(jump)
                if !labels.contains_key(&jump.label) =>
            {
                return Err(RuntimeError::new("invalid_jump", path, &jump.label));
            }
            Instruction::Spawn(call)
            | Instruction::CallDirect(call)
            | Instruction::CallValue(call)
            | Instruction::TailCallDirect(call)
                if call.arg_count < 0 || call.result_count < 0 =>
            {
                return Err(RuntimeError::new(
                    "invalid_call",
                    path,
                    "negative call count",
                ));
            }
            Instruction::Return(ret)
                if ret.result_count != function.declaration.signature.results.len() as i64 =>
            {
                return Err(RuntimeError::new(
                    "invalid_return",
                    path,
                    "result count mismatch",
                ));
            }
            _ => {}
        }
    }
    slots::verify(function, labels, path)
}

fn instruction_arity(
    instruction: &Instruction,
    path: &str,
) -> Result<(usize, usize), RuntimeError> {
    if let Some(arity) = instruction.fixed_arity() {
        return Ok(arity);
    }
    use Instruction::*;
    let arity = match instruction {
        MakeSlice(value) => (if value.has_capacity { 2 } else { 1 }, 1),
        MakeMap(value) => (
            value.entry_count as usize * 2 + usize::from(value.has_capacity),
            1,
        ),
        Append(value) => (value.count as usize + 1, 1),
        MakeStruct(value) => (value.fields.len(), 1),
        MakeSequence(value) => (value.element_count as usize, 1),
        CallInterface(value) => (value.arg_count as usize + 1, value.result_count as usize),
        CallIntrinsic(value) => (value.arg_count as usize, value.result_count as usize),
        Spawn(value) => (value.arg_count as usize + 1, 0),
        CallDirect(value) => (value.arg_count as usize, value.result_count as usize),
        TailCallDirect(value) => (value.arg_count as usize, 0),
        CallValue(value) => (value.arg_count as usize + 1, value.result_count as usize),
        Return(value) => (value.result_count as usize, 0),
        CallFfi(value) => (value.arg_count as usize, value.result_count as usize),
        _ => {
            return Err(RuntimeError::new(
                "invalid_operand",
                path,
                "missing payload operand arity",
            ));
        }
    };
    Ok(arity)
}
