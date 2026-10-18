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
}

impl Revision {
    pub(crate) fn function_by_index(
        &self,
        index: usize,
    ) -> Result<Arc<PreparedFunction>, RuntimeError> {
        self.program.function_by_index(index)
    }
}

mod constant;
pub(crate) use constant::PreparedConstant;
mod construct;
pub(crate) use construct::StructLayout;
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
    pub module: Arc<str>,
    pub name: Arc<str>,
    pub local_types: Vec<crate::types::TypeIdentity>,
    pub addressable_locals: Vec<bool>,
    pub result_types: Vec<crate::types::TypeIdentity>,
    pub result_locals: Vec<usize>,
    pub execution: Vec<PreparedInstruction>,
    pub modes: Vec<u16>,
    pub costs: Vec<u32>,
    pub blocks: Vec<wire::BasicBlock>,
    pub block_at_pc: Vec<u32>,
    pub operand_types: Vec<Option<crate::types::TypeIdentity>>,
    pub type_dispatches: Vec<PreparedTypeDispatch>,
    pub aggregates: Vec<StructLayout>,
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
    /// Map iterator storage is indexed by the prepared local used by the
    /// instruction. The instruction payload keeps the source name for
    /// diagnostics, but execution never hashes that name on each iteration.
    pub map_iterator_indices: Vec<Option<usize>>,
    pub map_iterator_count: usize,
    // Only direct locals and non-scalar constants may need owner-side loading.
    pub lazy_slot_inputs: Vec<bool>,
    /// Interned precise guest roots at each instruction boundary. Bits address
    /// frame locals, prepared temporary slots, and captured upvalue cells in
    /// that order. The frame owner also visits captured cells directly.
    pub reference_maps: Arc<Vec<Vec<u64>>>,
    pub reference_at_pc: Arc<Vec<u32>>,
}

#[derive(Clone, Copy, Debug)]
pub(crate) enum PreparedInstruction {
    GetPath(usize),
    CompareBranch {
        operator: crate::operators::PreparedOperator,
        target: usize,
        when: bool,
    },
    LenCompareBranch {
        operator: crate::operators::PreparedOperator,
        target: usize,
        when: bool,
    },
    UpdateCompareBranch {
        update: crate::operators::PreparedOperator,
        comparison: crate::operators::PreparedOperator,
        target: usize,
        when: bool,
    },
    Construct(usize),
    TypeDispatch(usize),
    Intrinsic(wire::Intrinsic),
    Unary(crate::operators::PreparedOperator),
    Binary(crate::operators::PreparedOperator),
    MinMax {
        kind: crate::operators::MinMaxKind,
        choose_min: bool,
        arg_count: usize,
    },
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

fn prepare_reference_plan(function: &PreparedFunction) -> (Vec<Vec<u64>>, Vec<u32>) {
    let local_count = function.local_types.len();
    let slot_count = function.slot_types.len();
    let upvalue_count = function.upvalues.len();
    let upvalue_base = local_count + slot_count;
    let storage_count = upvalue_base + upvalue_count;
    let words = storage_count.div_ceil(64);
    let count = function.execution.len();
    if count == 0 {
        return (vec![vec![0; words]], vec![0]);
    }
    let mut uses = vec![vec![0_u64; words]; count];
    let mut defs = vec![vec![0_u64; words]; count];
    let mut successors = vec![Vec::<(usize, Option<usize>)>::new(); count];
    let set = |bitmap: &mut [u64], index: usize| {
        if index < storage_count {
            bitmap[index / 64] |= 1_u64 << (index % 64);
        }
    };
    let mut result_roots = vec![0_u64; words];
    for &index in &function.result_locals {
        set(&mut result_roots, index);
    }
    for (pc, execution) in function.execution.iter().enumerate() {
        let use_map = &mut uses[pc];
        let def_map = &mut defs[pc];
        for (word, value) in result_roots.iter().enumerate() {
            use_map[word] |= value;
        }
        for input in function.operands.inputs_at(pc) {
            match input.kind {
                0 => set(use_map, local_count + input.index as usize),
                2 => set(use_map, input.index as usize),
                _ => {}
            }
        }
        for &output in function.operands.outputs_at(pc) {
            if output & LOCAL_OUTPUT != 0 {
                let local = (output & !LOCAL_OUTPUT) as usize;
                // A direct result updates the same private storage as
                // StoreLocal. Keep its existing managed identity until
                // publication, including while a callee is suspended.
                set(use_map, local);
                set(def_map, local);
            } else {
                set(def_map, local_count + output as usize);
            }
        }
        let read_address = |address: &wire::AddressPayload, bitmap: &mut [u64]| {
            match address.kind.as_str() {
                "local" => {
                    if let Some(index) = function.locals.get(&address.local) {
                        set(bitmap, *index);
                    }
                }
                "upvalue" => {
                    if let Some(index) = function.upvalues.get(&address.upvalue) {
                        set(bitmap, upvalue_base + *index);
                    }
                }
                _ => {}
            }
            for segment in address.path.iter() {
                if segment.kind == "index"
                    && let Some(index) = function.locals.get(&segment.local)
                {
                    set(bitmap, *index);
                }
            }
        };
        match execution {
            PreparedInstruction::Local {
                index,
                store,
                rebind,
                ..
            } => {
                if *store {
                    if function.addressable_locals[*index] || !*rebind {
                        set(use_map, *index);
                    }
                    set(def_map, *index);
                } else {
                    set(use_map, *index);
                }
                if pc + 1 < count {
                    successors[pc].push((pc + 1, None));
                }
            }
            PreparedInstruction::Upvalue { index, .. } => {
                // Upvalue cells retain identity across stores and unwind; a
                // store therefore reads the cell even when its new payload is
                // scalar. The frame owner also visits these roots directly.
                set(use_map, upvalue_base + *index);
                if pc + 1 < count {
                    successors[pc].push((pc + 1, None));
                }
            }
            PreparedInstruction::TypeDispatch(index) => {
                let dispatch = &function.type_dispatches[*index];
                set(use_map, dispatch.subject);
                let kill = dispatch.fallback_local.filter(|local| {
                    if function.addressable_locals[*local] {
                        set(use_map, *local);
                        false
                    } else {
                        true
                    }
                });
                successors[pc].push((dispatch.fallback, kill));
                for case in &dispatch.cases {
                    let kill = case.binding.filter(|binding| {
                        if function.addressable_locals[*binding] {
                            set(use_map, *binding);
                            false
                        } else {
                            true
                        }
                    });
                    successors[pc].push((case.target, kill));
                }
            }
            PreparedInstruction::Jump {
                target,
                conditional,
                ..
            } => {
                successors[pc].push((*target, None));
                if *conditional && pc + 1 < count {
                    successors[pc].push((pc + 1, None));
                }
            }
            PreparedInstruction::CompareBranch { target, .. }
            | PreparedInstruction::LenCompareBranch { target, .. }
            | PreparedInstruction::UpdateCompareBranch { target, .. } => {
                successors[pc].push((*target, None));
                if pc + 1 < count {
                    successors[pc].push((pc + 1, None));
                }
            }
            PreparedInstruction::Operand => {
                // Return, panic, and tail call terminate the current linear
                // frame. All other wire operations resume at the next PC.
                let terminal = matches!(
                    function.code.descriptor(pc),
                    Some(
                        Instruction::Return(_)
                            | Instruction::Panic
                            | Instruction::TailCallDirect(_)
                    )
                );
                if !terminal && pc + 1 < count {
                    successors[pc].push((pc + 1, None));
                }
            }
            PreparedInstruction::Construct(_)
            | PreparedInstruction::GetPath(_)
            | PreparedInstruction::Intrinsic(_)
            | PreparedInstruction::Unary(_)
            | PreparedInstruction::Binary(_)
            | PreparedInstruction::MinMax { .. }
            | PreparedInstruction::Constant(_)
            | PreparedInstruction::Global { .. } => {
                if pc + 1 < count {
                    successors[pc].push((pc + 1, None));
                }
            }
        }
        if let Some(descriptor) = function.code.descriptor(pc) {
            match descriptor {
                Instruction::AddressOf(address) => read_address(address, use_map),
                Instruction::MakeClosure(closure) => {
                    for address in closure.captures.iter() {
                        read_address(address, use_map);
                    }
                }
                Instruction::Select(select) => {
                    if let Some(index) = function.locals.get(&select.index) {
                        set(def_map, *index);
                        if function.addressable_locals[*index] {
                            set(use_map, *index);
                        }
                    }
                    for case in select.cases.iter() {
                        if let Some(index) = function.locals.get(&case.channel) {
                            set(use_map, *index);
                        }
                        if !case.send.is_empty() {
                            if let Some(index) = function.locals.get(&case.send) {
                                set(use_map, *index);
                            }
                        } else {
                            for name in [&case.value, &case.ok] {
                                if !name.is_empty()
                                    && let Some(index) = function.locals.get(name)
                                    && function.addressable_locals[*index]
                                {
                                    set(use_map, *index);
                                }
                            }
                        }
                    }
                }
                _ => {}
            }
        }
    }
    let mut incoming = vec![vec![0_u64; words]; count + 1];
    incoming[count].copy_from_slice(&result_roots);
    loop {
        let mut changed = false;
        for pc in (0..count).rev() {
            let mut outgoing = vec![0_u64; words];
            for &(target, kill) in &successors[pc] {
                if target >= count {
                    continue;
                }
                for (word, value) in outgoing.iter_mut().enumerate() {
                    let mut edge = incoming[target][word];
                    if kill.is_some_and(|index| index / 64 == word) {
                        edge &= !(1_u64 << (kill.unwrap() % 64));
                    }
                    *value |= edge;
                }
            }
            for word in 0..words {
                let next = uses[pc][word] | (outgoing[word] & !defs[pc][word]);
                if incoming[pc][word] != next {
                    incoming[pc][word] = next;
                    changed = true;
                }
            }
        }
        if !changed {
            break;
        }
    }
    let mut maps = Vec::<Vec<u64>>::new();
    let mut interned = HashMap::<Vec<u64>, u32>::new();
    let mut at_pc = Vec::with_capacity(count + 1);
    for bitmap in incoming {
        let id = if let Some(id) = interned.get(&bitmap) {
            *id
        } else {
            let id = maps.len() as u32;
            interned.insert(bitmap.clone(), id);
            maps.push(bitmap);
            id
        };
        at_pc.push(id);
    }
    (maps, at_pc)
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
        for (module, artifact) in decoded.artifacts() {
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
                    .ok_or_else(|| RuntimeError::new("invalid_code", &path, "missing microcode"))?;
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
                    modes: Vec::new(),
                    costs: Vec::new(),
                    blocks: code.blocks.iter().cloned().collect(),
                    block_at_pc: Vec::new(),
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
                    map_iterator_indices: Vec::new(),
                    map_iterator_count: 0,
                    lazy_slot_inputs: Vec::new(),
                    reference_maps: Arc::default(),
                    reference_at_pc: Arc::default(),
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
                                        | Instruction::StringNextRuneIndex
                                        | Instruction::IndexConvert(_)
                                        | Instruction::CallDirect(_)
                                        | Instruction::MapIterNext(_)
                                        | Instruction::MinMax(_)
                                ) && (operands.outputs.len() == 1
                                    || matches!(
                                        instruction,
                                        Instruction::CallDirect(_) | Instruction::MapIterNext(_)
                                    ))
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
                        Instruction::MinMax(value) => {
                            decoded.types().resolve(module, &value.r#type)?;
                            value.arg_count >= 1 && matches!(value.operator.as_str(), "min" | "max")
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
                    function.modes.push(code.instructions[position].mode);
                    function.costs.push(code.instructions[position].cost);
                }
                let mut covered = 0usize;
                function.block_at_pc = vec![0; function.code.len()];
                for (block_index, block) in function.blocks.iter().enumerate() {
                    let first = block.first as usize;
                    let count = block.count as usize;
                    if first != covered
                        || count == 0
                        || first.saturating_add(count) > function.code.len()
                    {
                        return Err(RuntimeError::new(
                            "invalid_code",
                            &path,
                            "invalid basic block range",
                        ));
                    }
                    let cost = function.costs[first..first + count]
                        .iter()
                        .try_fold(0u32, |sum, value| sum.checked_add(*value))
                        .ok_or_else(|| {
                            RuntimeError::new("invalid_code", &path, "basic block cost overflow")
                        })?;
                    if cost != block.cost
                        || block.fallthrough != u32::MAX
                            && block.fallthrough as usize >= function.blocks.len()
                    {
                        return Err(RuntimeError::new(
                            "invalid_code",
                            &path,
                            "invalid basic block metadata",
                        ));
                    }
                    function.block_at_pc[first..first + count].fill(block_index as u32);
                    covered += count;
                }
                if !function.code.is_empty()
                    && (function.blocks.is_empty() || covered != function.code.len())
                {
                    return Err(RuntimeError::new(
                        "invalid_code",
                        &path,
                        "basic blocks do not cover executable code",
                    ));
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
                                steps.push(PreparedField {
                                    indirect,
                                    index: index as usize,
                                });
                                typ = decoded.types().resolve(owner, &field.r#type)?;
                            }
                            let index = function.field_paths.len();
                            function.field_paths.push(steps);
                            PreparedInstruction::GetPath(index)
                        }
                        CompareBranch(payload) => PreparedInstruction::CompareBranch {
                            operator: crate::operators::PreparedOperator::parse(&payload.operator)?,
                            target: *labels.get(&payload.label).ok_or_else(|| {
                                RuntimeError::new("invalid_jump", &path, &payload.label)
                            })?,
                            when: payload.when,
                        },
                        LenCompareBranch(payload) => PreparedInstruction::LenCompareBranch {
                            operator: crate::operators::PreparedOperator::parse(&payload.operator)?,
                            target: *labels.get(&payload.label).ok_or_else(|| {
                                RuntimeError::new("invalid_jump", &path, &payload.label)
                            })?,
                            when: payload.when,
                        },
                        UpdateCompareBranch(payload) => PreparedInstruction::UpdateCompareBranch {
                            update: crate::operators::PreparedOperator::parse(&payload.update)?,
                            comparison: crate::operators::PreparedOperator::parse(
                                &payload.comparison,
                            )?,
                            target: *labels.get(&payload.label).ok_or_else(|| {
                                RuntimeError::new("invalid_jump", &path, &payload.label)
                            })?,
                            when: payload.when,
                        },
                        MakeStruct(payload) => {
                            let index = function.aggregates.len();
                            function.aggregates.push(StructLayout::prepare(
                                decoded.types(),
                                &decoded.types().resolve(module, &payload.r#type)?,
                                &payload.fields,
                            )?);
                            PreparedInstruction::Construct(index)
                        }
                        MakeSequence(payload) => {
                            construct::validate_aggregate_layout(
                                decoded.types(),
                                &decoded.types().resolve(module, &payload.r#type)?,
                            )?;
                            PreparedInstruction::Operand
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
                        MinMax(value) => {
                            let typ = decoded.types().resolve(module, &value.r#type)?;
                            let kind = crate::operators::min_max_kind(&typ, decoded.types())?;
                            let arg_count = usize::try_from(value.arg_count).map_err(|_| {
                                RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "min/max argument count is out of range",
                                )
                            })?;
                            if arg_count == 0 {
                                return Err(RuntimeError::new(
                                    "invalid_operand",
                                    &path,
                                    "min/max requires at least one argument",
                                ));
                            }
                            PreparedInstruction::MinMax {
                                kind,
                                choose_min: match value.operator.as_str() {
                                    "min" => true,
                                    "max" => false,
                                    _ => {
                                        return Err(RuntimeError::new(
                                            "invalid_operand",
                                            &path,
                                            "min/max operator must be min or max",
                                        ));
                                    }
                                },
                                arg_count,
                            }
                        }
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
                        Zero(value) | Convert(value) | TypeAssert(value) | TypeAssertOk(value)
                        | IndexConvert(value) => Some(&value.r#type),
                        UpdateCompareBranch(value) => Some(&value.r#type),
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
                            PreparedInstruction::LenCompareBranch {
                                operator: operation,
                                ..
                            } => operation,
                            _ => continue,
                        };
                        for (index, input) in operands.inputs.iter().enumerate() {
                            let length_type =
                                crate::types::TypeIdentity::Primitive(wire::PrimitiveInt);
                            let typ =
                                if matches!(function.code[pc], Instruction::LenCompareBranch(_))
                                    && index == 0
                                {
                                    &length_type
                                } else {
                                    match input.kind {
                                        0 => &function.slot_types[input.index as usize],
                                        2 => &function.local_types[input.index as usize],
                                        _ => &constant_types[input.index as usize],
                                    }
                                };
                            operation.bind(index, typ, decoded.types())?;
                        }
                    }
                    for (pc, prepared) in function.execution.iter_mut().enumerate() {
                        let PreparedInstruction::UpdateCompareBranch {
                            update, comparison, ..
                        } = prepared
                        else {
                            continue;
                        };
                        let inputs = function.operands.inputs_at(pc);
                        for (index, input) in inputs.iter().enumerate() {
                            let typ = match input.kind {
                                0 => &function.slot_types[input.index as usize],
                                2 => &function.local_types[input.index as usize],
                                _ => &constant_types[input.index as usize],
                            };
                            if index < 2 {
                                update.bind(index, typ, decoded.types())?;
                            }
                            if index == 0 {
                                comparison.bind(0, typ, decoded.types())?;
                            } else if index == 2 {
                                comparison.bind(1, typ, decoded.types())?;
                            }
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
            let mut iterator_indices = HashMap::new();
            function.map_iterator_indices = (0..function.execution.len())
                .map(|pc| match &function.code[pc] {
                    Instruction::MapIterInit(payload)
                    | Instruction::MapIterNext(payload)
                    | Instruction::MapIterClose(payload) => {
                        let next = iterator_indices.len();
                        Some(
                            *iterator_indices
                                .entry(payload.local.clone())
                                .or_insert(next),
                        )
                    }
                    _ => None,
                })
                .collect();
            function.map_iterator_count = iterator_indices.len();
            function.code.compact(&function.execution);
            let (maps, at_pc) = prepare_reference_plan(function);
            if at_pc.len() != function.execution.len() + 1
                || at_pc
                    .iter()
                    .any(|map| usize::try_from(*map).map_or(true, |index| index >= maps.len()))
            {
                return Err(RuntimeError::new(
                    "invalid_reference_plan",
                    format!("{}.{}", function.module, function.name),
                    "prepared function has an incomplete reference map",
                ));
            }
            function.reference_maps = Arc::new(maps);
            function.reference_at_pc = Arc::new(at_pc);
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

    pub(crate) fn function_by_index(
        &self,
        index: usize,
    ) -> Result<Arc<PreparedFunction>, RuntimeError> {
        self.function_table.get(index).cloned().ok_or_else(|| {
            RuntimeError::new(
                "missing_function",
                "program",
                format!("numeric function id {index} is out of range"),
            )
        })
    }

    pub(crate) fn function_index(&self, module: &str, id: &str) -> Option<usize> {
        self.function_lookup
            .get(module)
            .and_then(|functions| functions.get(id))
            .copied()
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
        MinMax(value) => (value.arg_count as usize, 1),
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

#[cfg(test)]
mod reference_plan_tests {
    use super::*;

    fn empty_operands(count: usize) -> PreparedOperands {
        let mut operands = PreparedOperands::default();
        for _ in 0..count {
            operands.push(wire::RegisterFlow::default());
        }
        operands
    }

    fn prepared_function(
        local_types: Vec<crate::types::TypeIdentity>,
        locals: HashMap<String, usize>,
        upvalues: HashMap<String, usize>,
        execution: Vec<PreparedInstruction>,
        code: Vec<Instruction>,
        operands: PreparedOperands,
    ) -> PreparedFunction {
        let mut prepared_code = PreparedCode::default();
        for instruction in code {
            prepared_code.push(instruction);
        }
        let operand_count = execution.len();
        PreparedFunction {
            index: 0,
            module: "test".into(),
            name: "main".into(),
            addressable_locals: vec![false; local_types.len()],
            local_types,
            result_types: Vec::new(),
            result_locals: Vec::new(),
            execution,
            modes: vec![0; operand_count],
            costs: vec![1; operand_count],
            blocks: Vec::new(),
            block_at_pc: vec![0; operand_count],
            operand_types: vec![None; operand_count],
            type_dispatches: Vec::new(),
            aggregates: Vec::new(),
            field_paths: Vec::new(),
            globals: Vec::new(),
            targets: Vec::new(),
            locations: Vec::new(),
            declaration: wire::Function::default(),
            code: prepared_code,
            opcodes: Vec::new(),
            locals,
            upvalues,
            slot_types: Vec::new(),
            operands,
            map_iterator_indices: Vec::new(),
            map_iterator_count: 0,
            lazy_slot_inputs: Vec::new(),
            reference_maps: Arc::default(),
            reference_at_pc: Arc::default(),
        }
    }

    fn is_live(function: &PreparedFunction, pc: usize, slot: usize) -> bool {
        let (maps, at_pc) = prepare_reference_plan(function);
        maps[at_pc[pc] as usize]
            .get(slot / 64)
            .is_some_and(|word| word & (1_u64 << (slot % 64)) != 0)
    }

    #[test]
    fn direct_result_keeps_destination_identity_live_until_publication() {
        let mut operands = PreparedOperands::default();
        operands.push(wire::RegisterFlow {
            outputs: crate::contract::GoSlice(Some(vec![LOCAL_OUTPUT])),
            ..Default::default()
        });
        let function = prepared_function(
            vec![crate::types::TypeIdentity::Any],
            HashMap::from([(String::from("result"), 0)]),
            HashMap::new(),
            vec![PreparedInstruction::Operand],
            vec![Instruction::CallDirect(wire::CallPayload::default())],
            operands,
        );
        assert!(is_live(&function, 0, 0));
    }

    #[test]
    fn local_fallthrough_preserves_later_reference() {
        let local_function = prepared_function(
            vec![crate::types::TypeIdentity::Any; 2],
            HashMap::from([(String::from("x"), 0), (String::from("y"), 1)]),
            HashMap::new(),
            vec![
                PreparedInstruction::Constant(0),
                PreparedInstruction::Local {
                    index: 0,
                    move_source: u32::MAX,
                    store: true,
                    rebind: true,
                },
                PreparedInstruction::Local {
                    index: 1,
                    move_source: u32::MAX,
                    store: false,
                    rebind: false,
                },
            ],
            vec![
                Instruction::Const(wire::ConstPayload::default()),
                Instruction::StoreLocal(wire::LocalPayload::default()),
                Instruction::LoadLocal(wire::LocalPayload::default()),
            ],
            empty_operands(3),
        );
        assert!(is_live(&local_function, 0, 1));
    }

    #[test]
    fn upvalue_fallthrough_preserves_later_reference() {
        let upvalue_function = prepared_function(
            Vec::new(),
            HashMap::new(),
            HashMap::from([(String::from("x"), 0), (String::from("y"), 1)]),
            vec![
                PreparedInstruction::Constant(0),
                PreparedInstruction::Upvalue {
                    index: 0,
                    store: true,
                },
                PreparedInstruction::Upvalue {
                    index: 1,
                    store: false,
                },
            ],
            vec![
                Instruction::Const(wire::ConstPayload::default()),
                Instruction::StoreUpvalue(wire::UpvaluePayload::default()),
                Instruction::LoadUpvalue(wire::UpvaluePayload::default()),
            ],
            empty_operands(3),
        );
        assert!(is_live(&upvalue_function, 0, 1));
    }

    #[test]
    fn terminal_instructions_do_not_retain_unreachable_successors() {
        for terminal in [
            Instruction::Return(wire::ReturnPayload::default()),
            Instruction::Panic,
            Instruction::TailCallDirect(wire::CallPayload::default()),
        ] {
            let function = prepared_function(
                vec![crate::types::TypeIdentity::Any],
                HashMap::from([(String::from("x"), 0)]),
                HashMap::new(),
                vec![
                    PreparedInstruction::Operand,
                    PreparedInstruction::Local {
                        index: 0,
                        move_source: u32::MAX,
                        store: false,
                        rebind: false,
                    },
                ],
                vec![
                    terminal,
                    Instruction::LoadLocal(wire::LocalPayload::default()),
                ],
                empty_operands(2),
            );
            let (maps, at_pc) = prepare_reference_plan(&function);
            let live = |pc| {
                let map = &maps[at_pc[pc] as usize];
                map[0] & 1 != 0
            };
            assert!(!live(0), "terminal retained an unreachable successor root");
            assert!(live(1), "reachable local use was omitted from its boundary");
        }
    }
}
