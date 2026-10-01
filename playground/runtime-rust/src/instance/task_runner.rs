//! Task-private instructions shared by the serial owner and native worker
//! slices. Shared storage, lifecycle changes and potentially blocking
//! operations use the owner's instruction transaction.

use super::{frame::Local, scheduler::Task, *};
#[cfg(not(target_arch = "wasm32"))]
use crate::executor_pool::{Next, Pool};
use crate::program::{Instruction, PreparedConstant, PreparedInstruction};
#[cfg(not(target_arch = "wasm32"))]
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
        mpsc,
    },
    task::Wake as _,
};

#[cfg(not(target_arch = "wasm32"))]
pub(super) const INSTRUCTION_QUANTUM: usize = 1024;

#[cfg(not(target_arch = "wasm32"))]
pub(crate) struct TaskRun {
    pub(super) task: Task,
    pub(super) steps: usize,
}

#[cfg(not(target_arch = "wasm32"))]
pub(crate) struct TaskBatch {
    receiver: mpsc::Receiver<(usize, TaskRun)>,
    runs: Vec<Option<TaskRun>>,
}

#[cfg(not(target_arch = "wasm32"))]
impl TaskBatch {
    pub(crate) fn try_complete(&mut self) -> Result<Option<Vec<TaskRun>>, RuntimeError> {
        loop {
            match self.receiver.try_recv() {
                Ok((index, run)) => self.runs[index] = Some(run),
                Err(mpsc::TryRecvError::Empty) => break,
                Err(mpsc::TryRecvError::Disconnected) if self.runs.iter().any(Option::is_none) => {
                    return Err(RuntimeError::new(
                        "executor",
                        "task",
                        "task worker stopped without a result",
                    ));
                }
                Err(mpsc::TryRecvError::Disconnected) => break,
            }
        }
        if self.runs.iter().any(Option::is_none) {
            return Ok(None);
        }
        Ok(Some(
            std::mem::take(&mut self.runs)
                .into_iter()
                .map(Option::unwrap)
                .collect(),
        ))
    }

    fn wait(mut self) -> Result<Vec<TaskRun>, RuntimeError> {
        let missing = self.runs.iter().filter(|run| run.is_none()).count();
        for _ in 0..missing {
            let (index, run) = self.receiver.recv().map_err(|_| {
                RuntimeError::new("executor", "task", "task worker stopped without a result")
            })?;
            self.runs[index] = Some(run);
        }
        Ok(self.runs.into_iter().map(Option::unwrap).collect())
    }
}

pub(super) fn can_execute_private_instruction(task: &Task) -> bool {
    if task.pending_write.is_some()
        || task.selection_completion.is_some()
        || task.retry_instruction
        || task.pending_error.is_some()
        || task.instruction_active
        || task.frames.is_empty()
    {
        return false;
    }
    let frame = task.frames.last().unwrap();
    if frame.panic.is_some()
        || frame.continuation.is_some()
        || frame.returning.is_some()
        || frame.after_init.is_some()
        || frame.pc >= frame.prepared.code.len()
    {
        return false;
    }
    if frame.prepared.lazy_slot_inputs[frame.pc]
        && (0..frame.operand_count(frame.pc))
            .any(|offset| frame.operand_from_end(frame.pc, offset).is_none())
    {
        return false;
    }
    match frame.prepared.execution[frame.pc] {
        PreparedInstruction::GetPath(index) => {
            !frame.prepared.field_paths[index].iter().any(|step| step.indirect)
        }
        PreparedInstruction::Constant(index) => matches!(
            frame.revision.program.constant_data[index],
            PreparedConstant::Scalar(_)
        ),
        PreparedInstruction::Local {
            index,
            store,
            rebind,
            ..
        } => match &frame.locals[index] {
            Local::Private(value) if !store => !matches!(value.get().data, Data::Uninitialized),
            Local::Private(value) if !rebind => frame.operand_from_end(frame.pc, 0).is_some_and(|input| {
                input.typ == value.get().typ
            }),
            _ => false,
        },
        PreparedInstruction::Jump { conditional, .. } => {
            !conditional || frame.operand_from_end(frame.pc, 0).is_some_and(|value| matches!(value.data, Data::Bool(_)))
        }
        PreparedInstruction::Unary(_) => frame.operand_count(frame.pc) != 0,
        PreparedInstruction::Binary(operator) | PreparedInstruction::CompareBranch { operator, .. } => {
            frame.operand_count(frame.pc) >= 2
                && !(operator.operator == crate::operators::Operator::Add
                    && matches!(
                        (&frame.operand_from_end(frame.pc, 1).unwrap().data, &frame.operand_from_end(frame.pc, 0).unwrap().data),
                        (Data::String(_), Data::String(_))
                    ))
        }
        PreparedInstruction::Operand => match &frame.prepared.code[frame.pc] {
            Instruction::LoadIndex => frame.operand_count(frame.pc) >= 2
                && frame.operand_from_end(frame.pc, 1).is_some_and(|value| matches!(value.data, Data::String(_) | Data::Array(_))),
            Instruction::Pop => frame.operand_count(frame.pc) != 0,
            Instruction::Zero(_) => matches!(
                frame.prepared.operand_types[frame.pc],
                Some(TypeIdentity::Any | TypeIdentity::Primitive(_) | TypeIdentity::Pointer(_) | TypeIdentity::Slice(_))
            ),
            Instruction::LoadField(payload) => frame.operand_from_end(frame.pc, 0).is_some_and(|value| {
                matches!(&value.data, Data::Struct(fields) if fields.contains_key(&payload.field))
            }),
            Instruction::Len | Instruction::Cap => frame.operand_from_end(frame.pc, 0).is_some_and(|value| {
                matches!(value.data, Data::String(_) | Data::Array(_) | Data::Slice(_) | Data::Nil)
            }),
            Instruction::StringRuneAt | Instruction::StringNextRuneIndex => {
                frame.operand_count(frame.pc) >= 2
                    && matches!(frame.operand_from_end(frame.pc, 1).unwrap().data, Data::String(_))
                    && frame.operand_from_end(frame.pc, 0).is_some_and(|value| value.integer().is_ok())
            }
            Instruction::MapIterClose(_) => true,
            _ => false,
        },
        PreparedInstruction::Intrinsic(_)
        | PreparedInstruction::Construct(_)
        | PreparedInstruction::Upvalue { .. }
        | PreparedInstruction::Global { .. }
        | PreparedInstruction::TypeDispatch(_) => false,
    }
}

#[cfg(not(target_arch = "wasm32"))]
pub(super) fn run_private_batch(
    pool: &Arc<Pool>,
    batch: Vec<(Task, usize)>,
    types: TypeRegistry,
    observer: Option<&Arc<dyn Fn(u64, bool) + Send + Sync>>,
) -> Result<Vec<TaskRun>, (RuntimeError, Vec<Task>)> {
    start_private_batch(pool, batch, types, observer, None)?
        .wait()
        .map_err(|error| (error, Vec::new()))
}

#[cfg(not(target_arch = "wasm32"))]
pub(super) fn start_private_batch(
    pool: &Arc<Pool>,
    batch: Vec<(Task, usize)>,
    types: TypeRegistry,
    observer: Option<&Arc<dyn Fn(u64, bool) + Send + Sync>>,
    wake: Option<Arc<crate::ffi::Wake>>,
) -> Result<TaskBatch, (RuntimeError, Vec<Task>)> {
    let (sender, receiver) = mpsc::channel();
    let inputs = batch
        .into_iter()
        .map(|input| Arc::new(Mutex::new(Some(input))))
        .collect::<Vec<_>>();
    let mut jobs = Vec::with_capacity(inputs.len());
    let remaining = Arc::new(AtomicUsize::new(inputs.len()));
    for (index, input) in inputs.iter().cloned().enumerate() {
        let worker_input = input.clone();
        let sender = sender.clone();
        let types = types.clone();
        let observer = observer.cloned();
        let remaining = remaining.clone();
        let wake = wake.clone();
        let job = match pool.job(move || {
            let Some((task, quantum)) = worker_input.lock().unwrap().take() else {
                return Next::Done;
            };
            if let Some(observer) = &observer {
                observer(task.id, true);
            }
            let run = run_private_slice(task, quantum, types.clone());
            if let Some(observer) = &observer {
                observer(run.task.id, false);
            }
            let _ = sender.send((index, run));
            if remaining.fetch_sub(1, Ordering::AcqRel) == 1
                && let Some(wake) = &wake
            {
                wake.signal();
            }
            Next::Done
        }) {
            Ok(job) => job,
            Err(error) => {
                let tasks = inputs
                    .iter()
                    .filter_map(|input| input.lock().unwrap().take().map(|(task, _)| task))
                    .collect::<Vec<_>>();
                return Err((error, tasks));
            }
        };
        jobs.push(job);
    }
    drop(sender);
    for job in &jobs {
        job.wake_by_ref();
    }
    Ok(TaskBatch {
        receiver,
        runs: (0..jobs.len()).map(|_| None).collect(),
    })
}

#[cfg(not(target_arch = "wasm32"))]
fn run_private_slice(mut task: Task, quantum: usize, mut types: TypeRegistry) -> TaskRun {
    types.use_context(task.frames.last().unwrap().revision.program.decoded.types());
    let steps = execute_private_slice(&mut task, quantum, &types);
    TaskRun { task, steps }
}

pub(super) fn execute_private_slice(
    task: &mut Task,
    quantum: usize,
    types: &TypeRegistry,
) -> usize {
    let mut steps = 0usize;
    if !can_execute_private_instruction(task) {
        return 0;
    }
    while steps < quantum {
        let remaining = task.step_grant.as_ref().map_or(0, |grant| grant.remaining);
        if remaining == 0 {
            break;
        }
        if !try_execute_private_instruction(task, types) {
            break;
        }
        task.step_grant.as_mut().unwrap().remaining -= 1;
        task.scheduling_phase =
            ((usize::from(task.scheduling_phase) + 1) % usize::from(TASK_POLL_INTERVAL)) as u8;
        task.transient_roots.clear();
        steps += 1;
    }
    steps
}

pub(super) fn try_execute_private_instruction(task: &mut Task, types: &TypeRegistry) -> bool {
    let frame = task.frames.last_mut().unwrap();
    let pc = frame.pc;
    let function = &frame.prepared;
    if pc >= function.execution.len() {
        return false;
    }
    // Every fallible check precedes the commit. Returning false must leave the
    // continuation and accounting untouched so the owner can retry normally.
    let popped = match function.execution[pc] {
        PreparedInstruction::Local { store: true, .. }
        | PreparedInstruction::Jump {
            conditional: true, ..
        }
        | PreparedInstruction::Unary(_)
        | PreparedInstruction::GetPath(_) => 1,
        PreparedInstruction::Binary(_) | PreparedInstruction::CompareBranch { .. } => 2,
        PreparedInstruction::Operand => match function.code[pc] {
            Instruction::Pop | Instruction::LoadField(_) | Instruction::Len | Instruction::Cap => 1,
            Instruction::LoadIndex
            | Instruction::StringRuneAt
            | Instruction::StringNextRuneIndex => 2,
            _ => 0,
        },
        _ => 0,
    };
    if frame.operand_count(frame.pc) < popped {
        return false;
    }
    let mut next_pc = pc + 1;
    let mut result = None;
    match function.execution[pc] {
        PreparedInstruction::Constant(index) => {
            let PreparedConstant::Scalar(value) = &frame.revision.program.constant_data[index]
            else {
                return false;
            };
            result = Some(value.clone());
        }
        PreparedInstruction::Local {
            index,
            move_source,
            store,
            rebind,
        } => {
            if store {
                if rebind {
                    return false;
                }
                let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                    return false;
                };
                let Ok(bytes) = value.logical_bytes().and_then(|bytes| {
                    bytes.checked_add(128).ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "local", "logical size overflow")
                    })
                }) else {
                    return false;
                };
                let Local::Private(local) = &frame.locals[index] else {
                    return false;
                };
                if local.get().typ != value.typ {
                    return false;
                }
                let value = if move_source == u32::MAX {
                    value.clone()
                } else {
                    frame.slot_values[move_source as usize].take().unwrap()
                };
                let Local::Private(local) = &mut frame.locals[index] else {
                    unreachable!()
                };
                if let Err((_, value)) = local.replace(value, bytes) {
                    // No owner safepoint or GC occurs inside replacement. A
                    // rejected charge returns ownership before retrying.
                    if move_source != u32::MAX {
                        frame.slot_values[move_source as usize] = Some(value);
                    }
                    return false;
                }
            } else {
                let Local::Private(value) = &frame.locals[index] else {
                    return false;
                };
                if matches!(value.get().data, Data::Uninitialized) {
                    return false;
                }
                result = Some(value.get().clone());
            }
        }
        PreparedInstruction::Jump {
            target,
            conditional,
            negate,
        } => {
            let jump = if conditional {
                let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                    return false;
                };
                let Data::Bool(value) = value.data else {
                    return false;
                };
                value != negate
            } else {
                true
            };
            if jump {
                next_pc = target;
            }
        }
        PreparedInstruction::GetPath(index) => {
            let Some(mut value) = frame.operand_from_end(pc, 0) else {
                return false;
            };
            for step in &function.field_paths[index] {
                if step.indirect {
                    return false;
                }
                let Data::Struct(fields) = &value.data else {
                    return false;
                };
                let Some(field) = fields.field_at(step.index) else {
                    return false;
                };
                value = field;
            }
            result = Some(value.clone());
        }
        PreparedInstruction::Unary(operator) => {
            let Some(value) = frame.operand_from_end(frame.pc, 0).cloned() else {
                return false;
            };
            let Ok(value) = crate::operators::unary(operator, value, types) else {
                return false;
            };
            result = Some(value);
        }
        PreparedInstruction::Binary(operator)
        | PreparedInstruction::CompareBranch { operator, .. } => {
            if frame.operand_count(pc) < 2 {
                return false;
            }
            let Some(left) = frame.operand_from_end(pc, 1) else {
                return false;
            };
            let Some(right) = frame.operand_from_end(pc, 0) else {
                return false;
            };
            if operator.operator == crate::operators::Operator::Add
                && matches!(
                    (&left.data, &right.data),
                    (Data::String(_), Data::String(_))
                )
            {
                return false;
            }
            let Ok(value) = crate::operators::binary(operator, left, right, types) else {
                return false;
            };
            if let PreparedInstruction::CompareBranch { target, when, .. } = function.execution[pc]
            {
                let Data::Bool(matched) = value.data else {
                    return false;
                };
                if matched == when {
                    next_pc = target;
                }
            } else {
                result = Some(value);
            }
        }
        PreparedInstruction::Operand => match &function.code[pc] {
            Instruction::Pop => {}
            Instruction::Zero(_) => {
                let Some(typ) = function.operand_types[pc].as_ref() else {
                    return false;
                };
                let data = match typ {
                    TypeIdentity::Any | TypeIdentity::Pointer(_) | TypeIdentity::Slice(_) => {
                        Data::Nil
                    }
                    TypeIdentity::Primitive(wire::PrimitiveBool) => Data::Bool(false),
                    TypeIdentity::Primitive(wire::PrimitiveString) => {
                        Data::String((&[][..]).into())
                    }
                    TypeIdentity::Primitive(primitive)
                        if (wire::PrimitiveInt..=wire::PrimitiveInt64).contains(primitive) =>
                    {
                        Data::Integer(0)
                    }
                    TypeIdentity::Primitive(primitive)
                        if (wire::PrimitiveUint..=wire::PrimitiveUintptr).contains(primitive) =>
                    {
                        Data::Unsigned(0)
                    }
                    TypeIdentity::Primitive(wire::PrimitiveFloat32 | wire::PrimitiveFloat64) => {
                        Data::Float(0.0)
                    }
                    TypeIdentity::Primitive(
                        wire::PrimitiveComplex64 | wire::PrimitiveComplex128,
                    ) => Data::Complex {
                        real: 0.0,
                        imag: 0.0,
                    },
                    TypeIdentity::Primitive(_) => Data::Nil,
                    _ => return false,
                };
                result = Some(Value {
                    typ: typ.clone(),
                    data,
                });
            }
            Instruction::LoadField(payload) => {
                let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                    return false;
                };
                let Data::Struct(fields) = &value.data else {
                    return false;
                };
                let Some(value) = fields.get(&payload.field).cloned() else {
                    return false;
                };
                result = Some(value);
            }
            Instruction::LoadIndex => {
                let Some(object) = frame.operand_from_end(pc, 1) else {
                    return false;
                };
                let Some(key) = frame.operand_from_end(pc, 0) else {
                    return false;
                };
                let Ok(index) = key.integer().and_then(|index| {
                    usize::try_from(index)
                        .map_err(|_| RuntimeError::new("panic", "index", "negative index"))
                }) else {
                    return false;
                };
                result = match &object.data {
                    Data::String(bytes) => bytes.get(index).map(|byte| Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                        data: Data::Unsigned(u64::from(*byte)),
                    }),
                    Data::Array(values) => values.get(index).cloned(),
                    _ => return false,
                };
                if result.is_none() {
                    return false;
                }
            }
            Instruction::Len | Instruction::Cap => {
                let capacity = matches!(function.code[pc], Instruction::Cap);
                let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                    return false;
                };
                let pointer_array_length = match types.pointer_element(&value.typ) {
                    Ok(Some(element)) => match types.node(&element) {
                        Ok(Some((_, node))) if node.kind == wire::Array => Some(node.length),
                        Ok(_) => None,
                        Err(_) => return false,
                    },
                    Ok(None) => None,
                    Err(_) => return false,
                };
                let length = match (&value.data, pointer_array_length) {
                    (_, Some(length)) => length,
                    (Data::String(bytes), None) if !capacity => bytes.len() as i64,
                    (Data::Array(values), None) => values.len() as i64,
                    (Data::Slice(slice), None) => {
                        if capacity {
                            slice.capacity as i64
                        } else {
                            slice.length as i64
                        }
                    }
                    (Data::Nil, None) => 0,
                    _ => return false,
                };
                result = Some(Value::int(length));
            }
            Instruction::StringRuneAt | Instruction::StringNextRuneIndex => {
                if frame.operand_count(pc) < 2 {
                    return false;
                }
                let Some(Value {
                    data: Data::String(bytes),
                    ..
                }) = frame.operand_from_end(pc, 1)
                else {
                    return false;
                };
                let Ok(index) =
                    usize::try_from(match frame.operand_from_end(pc, 0).unwrap().integer() {
                        Ok(index) => index,
                        Err(_) => return false,
                    })
                else {
                    return false;
                };
                if index >= bytes.len() {
                    return false;
                }
                let (rune, width) = crate::value::decode_rune(&bytes[index..]);
                let next = matches!(function.code[pc], Instruction::StringNextRuneIndex);
                result = Some(if next {
                    Value::int((index + width) as i64)
                } else {
                    Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveInt32),
                        data: Data::Integer(i64::from(rune)),
                    }
                });
            }
            Instruction::MapIterClose(payload) => {
                frame.map_iterators.remove(&payload.local);
            }
            _ => return false,
        },
        PreparedInstruction::Intrinsic(_)
        | PreparedInstruction::Construct(_)
        | PreparedInstruction::Upvalue { .. }
        | PreparedInstruction::Global { .. }
        | PreparedInstruction::TypeDispatch(_) => return false,
    }
    frame.begin_slots(pc);
    frame.slot_input = 0;
    frame.pc = next_pc;
    if let Some(value) = result {
        frame.push_result(value);
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use test_helpers::slot_code;

    #[test]
    fn field_reads_follow_storage_layout_with_repeated_blank_fields() {
        let program = test_helpers::program_with_artifact(|artifact| {
            let integer = serde_json::json!({"kind":3,"primitive":3});
            let record = serde_json::json!({"kind":12,"node":"record"});
            artifact["type_table"] = serde_json::json!({"nodes":[{
                "id":"record","kind":12,"fields":[
                    {"name":"_","type":integer}, {"name":"_","type":integer},
                    {"name":"a","type":integer}, {"name":"b","type":integer}]}]});
            artifact["constants"] = serde_json::json!([
                {"id":"a","type":integer,"value":42},
                {"id":"b","type":integer,"value":99}]);
            artifact["functions"] = serde_json::json!([{
                "id":"fn.Main","signature":{"results":[integer]},
                "locals":[{"id":"record","type":record}],
                "code":slot_code(json!([integer,integer,record,record,integer]), &[
                    ("const",json!({"constant":"a"}),json!({"outputs":[0]})),
                    ("const",json!({"constant":"b"}),json!({"outputs":[1]})),
                    ("make_struct",json!({"type":record,"fields":["a","b"]}),json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]})),
                    ("store_local",json!({"local":"record"}),json!({"inputs":[[0,2]],"release":[2]})),
                    ("load_local",json!({"local":"record"}),json!({"outputs":[3]})),
                    ("load_field",json!({"field":"a"}),json!({"inputs":[[0,3]],"outputs":[4],"release":[3]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,4]],"release":[4]}))])}]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", vec![]).unwrap();
        assert_eq!(vm.poll_steps(4096).unwrap(), PollStatus::Ready);
        assert_eq!(vm.results()[0].integer().unwrap(), 42);
        vm.close().unwrap();
    }

    fn division_program(divisor: i64) -> Arc<Program> {
        test_helpers::program_with_artifact(|artifact| {
            artifact["constants"] = serde_json::json!([
                {"id":"a","type":{"kind":3,"primitive":3},"value":42},
                {"id":"b","type":{"kind":3,"primitive":3},"value":divisor}]);
            artifact["functions"] = serde_json::json!([{
                "id":"fn.Main", "signature":{"results":[{"kind":3,"primitive":3}]},
                "code":slot_code(json!(vec![json!({"kind":3,"primitive":3});3]), &[
                    ("const",json!({"constant":"a"}),json!({"outputs":[0]})),
                    ("const",json!({"constant":"b"}),json!({"outputs":[1]})),
                    ("binary",json!({"operator":"/"}),json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,2]],"release":[2]}))])}]);
        })
    }

    #[test]
    #[cfg(not(target_arch = "wasm32"))]
    fn private_slice_stops_at_its_granted_budget() {
        let mut vm = Instance::new(division_program(2), ExecutionLimits::default()).unwrap();
        vm.start("default", vec![]).unwrap();
        vm.running.step_grant = vm.scope_steps[&vm.running.scope].reserve(2, 64).unwrap();
        let run = run_private_slice(std::mem::take(&mut vm.running), 64, vm.types.clone());
        assert_eq!(run.steps, 2);
        vm.running = run.task;
        assert_eq!(vm.running.frames.last().unwrap().pc, 2);
        assert_eq!(
            vm.running
                .frames
                .last()
                .unwrap()
                .slot_values
                .iter()
                .flatten()
                .count(),
            2
        );
        vm.close().unwrap();
    }

    #[test]
    fn serial_private_execution_matches_owner_values_steps_and_accounting() {
        let program = division_program(2);
        let mut fast = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        let mut owner = Instance::new(program, ExecutionLimits::default()).unwrap();
        fast.start("default", vec![]).unwrap();
        owner.start("default", vec![]).unwrap();
        for _ in 0..3 {
            // An active transaction must use the owner path. Both paths still
            // have identical logical buffers and instruction budgets.
            let pc = owner.running.frames.last().unwrap().pc;
            owner.running.begin_instruction(pc);
            assert_eq!(fast.poll_steps(1).unwrap(), owner.poll_steps(1).unwrap());
            assert_eq!(fast.steps(), owner.steps());
            assert_eq!(fast.memory_stats(), owner.memory_stats());
        }
        for vm in [&mut fast, &mut owner] {
            assert_eq!(
                vm.running.frames.last().unwrap().slot_values[2]
                    .as_ref()
                    .unwrap()
                    .integer()
                    .unwrap(),
                21
            );
            assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
            vm.close().unwrap();
        }
    }

    #[test]
    fn private_string_index_matches_owner_bytes_and_failure_steps() {
        for index in [-1, 0, 1, 2] {
            let program = test_helpers::program_with_artifact(|artifact| {
                let text = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveString});
                let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
                let byte = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveUint8});
                artifact["constants"] = json!([
                    {"id":"text","type":text,"value":"é"},
                    {"id":"index","type":integer,"value":index}
                ]);
                artifact["functions"] = json!([{"id":"fn.Main","signature":{"results":[byte]},
                "code":slot_code(json!([byte]), &[
                    ("load_index",json!({}),json!({"inputs":[[1,0],[1,1]],"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))
                ])}]);
            });
            for private in [true, false] {
                let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
                vm.start("default", vec![]).unwrap();
                if !private {
                    vm.running.begin_instruction(0);
                }
                let result = vm.poll_steps(1);
                assert_eq!(vm.steps(), 1);
                if !(0..2).contains(&index) {
                    assert_ne!(result.unwrap(), PollStatus::Ready);
                    assert!(vm.running.frames.last().unwrap().slot_values[0].is_none());
                    assert_eq!(vm.poll_steps(1).unwrap_err().code, "panic");
                    assert_eq!(vm.steps(), 1);
                } else {
                    assert_ne!(result.unwrap(), PollStatus::Ready);
                    assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
                    assert!(matches!(vm.results()[0].data, Data::Unsigned(value)
                        if value == if index == 0 { 0xc3 } else { 0xa9 }));
                }
                vm.close().unwrap();
            }
        }
    }

    #[test]
    fn private_store_rejection_preserves_operands_local_and_accounting() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([{
                "id":"fn.Main", "locals":[{"id":"value","type":{"kind":3,"primitive":2}}],
                "code":slot_code(json!([{"kind":3,"primitive":2}]), &[
                    ("zero",json!({"type":{"kind":3,"primitive":2}}),json!({"outputs":[0]})),
                    ("store_local",json!({"local":"value"}),json!({"inputs":[[0,0]],"release":[0]})),
                    ("zero",json!({"type":{"kind":3,"primitive":2}}),json!({"outputs":[0]})),
                    ("store_local",json!({"local":"value"}),json!({"inputs":[[0,0]],"release":[0]})),
                    ("return",json!({}),json!({}))])
            }]);
        });
        let mut vm = Instance::new(
            program,
            ExecutionLimits {
                max_heap_bytes: 4096,
                ..Default::default()
            },
        )
        .unwrap();
        vm.start("default", vec![]).unwrap();
        assert_eq!(vm.poll_steps(3).unwrap(), PollStatus::Running);
        let frame = vm.running.frames.last_mut().unwrap();
        frame.slot_values[0] = Some(Value::string("x".repeat(8192)));
        let before = vm.heap_stats();
        assert!(can_execute_private_instruction(&vm.running));
        assert!(!try_execute_private_instruction(&mut vm.running, &vm.types));
        let frame = vm.running.frames.last().unwrap();
        assert_eq!(frame.pc, 3);
        assert!(
            matches!(&frame.slot_values[0].as_ref().unwrap().data, Data::String(bytes) if bytes.len() == 8192)
        );
        let Local::Private(local) = &frame.locals[0] else {
            panic!("private local")
        };
        assert!(matches!(&local.get().data,Data::String(bytes) if bytes.is_empty()));
        assert_eq!(vm.heap_stats().live_bytes, before.live_bytes);
        assert_eq!(
            vm.heap_stats().total_allocated_bytes,
            before.total_allocated_bytes
        );
        vm.close().unwrap();
    }

    #[test]
    fn last_use_store_restores_rejected_input_and_commits_ownership_once() {
        for release in [false, true] {
            let program = test_helpers::program_with_artifact(|artifact| {
                let contract: serde_json::Value =
                    serde_json::from_str(wire::CONTRACT_JSON).unwrap();
                let operation = |name: &str, descriptor, operands| {
                    let opcode = contract["spec"]["opcodes"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .position(|entry| entry["op"] == name)
                        .unwrap()
                        + 1;
                    serde_json::json!([opcode, descriptor, operands])
                };
                let string = serde_json::json!({"kind":3,"primitive":2});
                artifact["functions"] = serde_json::json!([{
                    "id":"fn.Main", "locals":[{"id":"value","type":string}],
                    "code":{
                        "types":[string],
                        "descriptors":{"type":[{"type":string}],"local":[{"local":"value"}],"return":[{}]},
                        "instructions":[operation("zero",0,0), operation("store_local",0,1),
                            operation("zero",0,0), operation("store_local",0,1), operation("return",0,2)],
                        "operands":[{"outputs":[0]}, {"inputs":[[0,0]],"release":if release {vec![0]} else {vec![]}}, {}]
                    }
                }]);
            });
            let mut vm = Instance::new(
                program,
                ExecutionLimits {
                    max_heap_bytes: 4096,
                    ..Default::default()
                },
            )
            .unwrap();
            vm.start("default", vec![]).unwrap();
            assert_eq!(vm.poll_steps(3).unwrap(), PollStatus::Running);
            vm.running.frames.last_mut().unwrap().slot_values[0] =
                Some(Value::string("x".repeat(8192)));
            let before = vm.heap_stats();
            assert!(!try_execute_private_instruction(&mut vm.running, &vm.types));
            let frame = vm.running.frames.last().unwrap();
            assert_eq!(frame.pc, 3);
            assert!(
                matches!(&frame.slot_values[0].as_ref().unwrap().data, Data::String(bytes) if bytes.len() == 8192)
            );
            let Local::Private(local) = &frame.locals[0] else {
                panic!("private local")
            };
            assert!(matches!(&local.get().data, Data::String(bytes) if bytes.is_empty()));
            assert_eq!(vm.heap_stats().live_bytes, before.live_bytes);
            assert_eq!(
                vm.heap_stats().total_allocated_bytes,
                before.total_allocated_bytes
            );
            vm.running.frames.last_mut().unwrap().slot_values[0] = Some(Value::string("ok"));
            assert!(try_execute_private_instruction(&mut vm.running, &vm.types));
            let frame = vm.running.frames.last().unwrap();
            assert_eq!(frame.pc, 4);
            assert_eq!(frame.slot_values[0].is_none(), release);
            let Local::Private(local) = &frame.locals[0] else {
                panic!("private local")
            };
            assert!(matches!(&local.get().data, Data::String(bytes) if bytes.as_ref() == b"ok"));
            vm.close().unwrap();
        }
    }

    #[test]
    fn private_binary_failure_preserves_inputs_and_success_preserves_buffer_charge() {
        for divisor in [0, 2] {
            let mut vm =
                Instance::new(division_program(divisor), ExecutionLimits::default()).unwrap();
            vm.start("default", vec![]).unwrap();
            vm.poll_steps(2).unwrap();
            assert_eq!(
                try_execute_private_instruction(&mut vm.running, &vm.types),
                divisor != 0
            );
            let frame = vm.running.frames.last().unwrap();
            if divisor == 0 {
                assert_eq!(frame.pc, 2);
                assert_eq!(
                    frame.slot_values[0].as_ref().unwrap().integer().unwrap(),
                    42
                );
                assert_eq!(frame.slot_values[1].as_ref().unwrap().integer().unwrap(), 0);
            } else {
                assert_eq!(frame.pc, 3);
                assert_eq!(
                    frame.slot_values[2].as_ref().unwrap().integer().unwrap(),
                    21
                );
                assert_eq!(frame.memory.popped, 0);
            }
            vm.close().unwrap();
        }
    }
}
