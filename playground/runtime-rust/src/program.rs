//! Immutable decoded instructions shared by instances. Preparation removes
//! labels and verifies operand references and stack heights before execution.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    loader::{DecodedImage, LoadLimits},
};
use serde::Deserialize;
use std::collections::{BTreeMap, HashMap, VecDeque};
use std::sync::Arc;

#[derive(Debug)]
pub(crate) struct Revision {
    pub generation: u64,
    pub program: Arc<Program>,
}

mod constant;
pub(crate) use constant::PreparedConstant;

#[derive(Clone, Debug, Deserialize)]
#[serde(tag = "op", content = "payload", rename_all = "snake_case")]
pub(crate) enum Instruction {
    Const(wire::ConstPayload),
    Zero(wire::TypePayload),
    Pop,
    Unary(wire::OperatorPayload),
    Binary(wire::OperatorPayload),
    LoadLocal(wire::LocalPayload),
    StoreLocal(wire::LocalPayload),
    LoadUpvalue(wire::UpvaluePayload),
    StoreUpvalue(wire::UpvaluePayload),
    LoadGlobal(wire::GlobalPayload),
    StoreGlobal(wire::GlobalPayload),
    Label(wire::LabelPayload),
    Jump(wire::JumpPayload),
    JumpIf(wire::JumpPayload),
    Return(wire::ReturnPayload),
    Panic,
    Recover,
    DeferPush(wire::DeferPayload),
    CallDirect(wire::CallPayload),
    TailCallDirect(wire::CallPayload),
    CallValue(wire::CallPayload),
    CallFfi(wire::CallFFIPayload),
    CallInterface(wire::CallInterfacePayload),
    CallIntrinsic(wire::CallIntrinsicPayload),
    MakeClosure(wire::ClosurePayload),
    MakeStruct(wire::MakeStructPayload),
    MakeSequence(wire::MakeSequencePayload),
    MakeSlice(wire::MakeSlicePayload),
    MakeMap(wire::MakeMapPayload),
    Slice,
    Append(wire::CountPayload),
    Copy,
    Clear,
    Delete,
    MapKeys,
    MapIterInit(wire::LocalPayload),
    MapIterNext(wire::LocalPayload),
    MapIterClose(wire::LocalPayload),
    LoadIndexOk,
    StringRuneAt,
    StringNextRuneIndex,
    TypeAssert(wire::TypePayload),
    TypeAssertOk(wire::TypePayload),
    LoadField(wire::FieldPayload),
    LoadIndex,
    StoreField(wire::FieldPayload),
    StoreIndex,
    AddressOf(wire::AddressPayload),
    LoadIndirect,
    StoreIndirect,
    Convert(wire::TypePayload),
    Len,
    Cap,
    LoadExport(wire::ExportPayload),
    InitModule(wire::InitModulePayload),
    Spawn(wire::CallPayload),
    MakeWaitable(wire::MakeWaitablePayload),
    Select(wire::SelectPayload),
    WaitableSend,
    WaitableRecv,
    WaitableRecvOk,
    WaitableTryRecv,
    WaitableTrySend,
    WaitableCanRecv,
    WaitableCanSend,
    WaitableClose,
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
    pub execution: Vec<PreparedInstruction>,
    pub operand_types: Vec<Option<crate::types::TypeIdentity>>,
    pub globals: Vec<String>,
    pub targets: Vec<Option<usize>>,
    pub locations: Vec<Vec<wire::Location>>,
    pub declaration: wire::Function,
    pub code: Vec<Instruction>,
    pub opcodes: Vec<String>,
    pub labels: HashMap<String, usize>,
    pub locals: HashMap<String, usize>,
    pub upvalues: HashMap<String, usize>,
    pub stack_limit: usize,
}

#[derive(Clone, Copy)]
pub(crate) enum PreparedInstruction {
    Intrinsic(wire::Intrinsic),
    Unary(crate::operators::Operator),
    Binary(crate::operators::Operator),
    Constant(usize),
    Local {
        index: usize,
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
    },
    Operand,
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
        for (module_index, (module, artifact)) in decoded.artifacts().iter().enumerate() {
            let constants = unique_ids(artifact.constants.iter().map(|value| &value.id), module)?;
            let globals = unique_ids(artifact.globals.iter().map(|value| &value.id), module)?;
            unique_ids(artifact.functions.iter().map(|value| &value.id), module)?;
            unique_ids(artifact.exports.iter().map(|value| &value.name), module)?;
            for constant in artifact.constants.iter() {
                prepared_constants
                    .insert((module.clone(), constant.id.clone()), constant_data.len());
                constant_data.push(PreparedConstant::decode(decoded.types(), module, constant)?);
            }
            for declaration in artifact.functions.iter() {
                let path = format!("{module}/{}", declaration.id);
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
                let mut function = PreparedFunction {
                    addressable_locals: vec![false; declaration.locals.len()],
                    targets: Vec::new(),
                    execution: Vec::new(),
                    operand_types: Vec::new(),
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
                    result_types: declaration
                        .signature
                        .results
                        .iter()
                        .map(|typ| decoded.types().resolve(module, typ))
                        .collect::<Result<_, _>>()?,
                    locations: Vec::new(),
                    declaration: declaration.clone(),
                    code: Vec::new(),
                    opcodes: Vec::new(),
                    labels: HashMap::new(),
                    locals,
                    upvalues,
                    stack_limit: 0,
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
                }
                for raw in declaration.instructions.iter() {
                    let mut encoded = serde_json::to_value(raw)?;
                    if raw.op == "defer_push" && raw.payload.is_none() {
                        encoded["payload"] = serde_json::json!({});
                    }
                    let instruction: Instruction =
                        serde_json::from_value(encoded).map_err(|error| {
                            RuntimeError::new("invalid_instruction", &path, error.to_string())
                        })?;
                    if let Instruction::Label(label) = instruction {
                        if label.label.is_empty()
                            || function
                                .labels
                                .insert(label.label, function.code.len())
                                .is_some()
                        {
                            return Err(RuntimeError::new(
                                "invalid_label",
                                &path,
                                "empty or duplicate label",
                            ));
                        }
                        continue;
                    }
                    let valid = match &instruction {
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
                    function.opcodes.push(raw.op.clone());
                }
                function.stack_limit = verify_stack(&function, &path)?;
                for instruction in &function.code {
                    use Instruction::*;
                    function.execution.push(match instruction {
                        CallIntrinsic(value) => PreparedInstruction::Intrinsic(
                            wire::Intrinsic::from_id(&value.id).unwrap(),
                        ),
                        Unary(value) => PreparedInstruction::Unary(
                            crate::operators::Operator::parse(&value.operator)?,
                        ),
                        Binary(value) => PreparedInstruction::Binary(
                            crate::operators::Operator::parse(&value.operator)?,
                        ),
                        Const(value) => PreparedInstruction::Constant(
                            prepared_constants[&(module.clone(), value.constant.clone())],
                        ),
                        LoadLocal(value) | StoreLocal(value) => PreparedInstruction::Local {
                            index: function.locals[&value.local],
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
                            target: function.labels[&value.label],
                            conditional: matches!(instruction, JumpIf(_)),
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
                function.declaration.instructions = Default::default();
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
            for instruction in &function.code {
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
            Arc::get_mut(&mut program.function_table[index])
                .unwrap()
                .targets = targets;
        }
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

fn verify_stack(function: &PreparedFunction, path: &str) -> Result<usize, RuntimeError> {
    let mut heights = vec![None; function.code.len() + 1];
    heights[0] = Some(0usize);
    let mut pending = VecDeque::from([0usize]);
    let mut maximum = 0;
    // Validate labels and counts even in unreachable instructions.
    for instruction in &function.code {
        match instruction {
            Instruction::Jump(jump) | Instruction::JumpIf(jump)
                if !function.labels.contains_key(&jump.label) =>
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
    while let Some(pc) = pending.pop_front() {
        let height = heights[pc].unwrap();
        if pc == function.code.len() {
            if height != 0 {
                return Err(RuntimeError::new(
                    "invalid_return",
                    path,
                    "fallthrough with operand values",
                ));
            }
            continue;
        }
        use Instruction::*;
        let instruction = &function.code[pc];
        let (pop, push) = match instruction {
            Const(_) | Zero(_) | LoadLocal(_) | LoadUpvalue(_) | LoadGlobal(_) | AddressOf(_)
            | MakeClosure(_) | LoadExport(_) | Recover => (0, 1),
            MapIterNext(_) => (0, 3),
            MapIterClose(_) => (0, 0),
            Pop | MapIterInit(_) | StoreLocal(_) | StoreUpvalue(_) | StoreGlobal(_) | JumpIf(_)
            | Panic | DeferPush(_) => (1, 0),
            Unary(_) | LoadField(_) | LoadIndirect | Convert(_) | TypeAssert(_) | Len | Cap
            | MapKeys => (1, 1),
            TypeAssertOk(_) => (1, 2),
            Binary(_) | LoadIndex | Copy | StringRuneAt | StringNextRuneIndex => (2, 1),
            LoadIndexOk => (2, 2),
            Slice => (4, 1),
            Clear => (1, 0),
            Delete => (2, 0),
            MakeSlice(value) => (if value.has_capacity { 2 } else { 1 }, 1),
            MakeMap(value) => (
                value.entry_count as usize * 2 + usize::from(value.has_capacity),
                1,
            ),
            Append(value) => (value.count as usize + 1, 1),
            StoreIndirect | StoreField(_) => (2, 0),
            StoreIndex => (3, 0),
            MakeStruct(value) => (value.fields.len(), 1),
            MakeSequence(value) => (value.element_count as usize, 1),
            CallFfi(_) => (2, 3),
            CallInterface(value) => (value.arg_count as usize + 1, value.result_count as usize),
            CallIntrinsic(value) => (value.arg_count as usize, value.result_count as usize),
            Spawn(value) => (value.arg_count as usize + 1, 0),
            MakeWaitable(_) | WaitableRecv | WaitableCanRecv | WaitableCanSend => (1, 1),
            WaitableRecvOk | WaitableTryRecv => (1, 2),
            WaitableTrySend => (2, 1),
            WaitableSend => (2, 0),
            WaitableClose => (1, 0),
            CallDirect(value) => (value.arg_count as usize, value.result_count as usize),
            TailCallDirect(value) => (value.arg_count as usize, 0),
            CallValue(value) => (value.arg_count as usize + 1, value.result_count as usize),
            Return(value) => (value.result_count as usize, 0),
            Jump(_) | InitModule(_) | Label(_) | Select(_) => (0, 0),
        };
        let next_height = height
            .checked_sub(pop)
            .and_then(|height| height.checked_add(push))
            .ok_or_else(|| {
                RuntimeError::new("invalid_stack", path, format!("stack underflow at {pc}"))
            })?;
        maximum = maximum.max(next_height);
        if matches!(instruction, Return(_) | TailCallDirect(_)) && height != pop {
            return Err(RuntimeError::new(
                "invalid_stack",
                path,
                "return leaves operands on stack",
            ));
        }
        let mut successors = Vec::with_capacity(2);
        match instruction {
            Jump(jump) => successors.push(function.labels[&jump.label]),
            JumpIf(jump) => {
                successors.push(function.labels[&jump.label]);
                successors.push(pc + 1);
            }
            Return(_) | Panic | TailCallDirect(_) => {}
            _ => successors.push(pc + 1),
        }
        for successor in successors {
            match heights[successor] {
                Some(previous) if previous != next_height => {
                    return Err(RuntimeError::new(
                        "invalid_stack",
                        path,
                        "inconsistent branch stack height",
                    ));
                }
                Some(_) => {}
                None => {
                    heights[successor] = Some(next_height);
                    pending.push_back(successor);
                }
            }
        }
    }
    Ok(maximum)
}
