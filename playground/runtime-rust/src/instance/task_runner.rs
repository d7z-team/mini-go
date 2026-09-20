//! Native task slices. A worker owns one complete task continuation and only
//! executes operations whose state belongs to that task. Shared storage,
//! lifecycle changes and potentially blocking operations return to the
//! instance owner as short control transactions.

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

pub(super) fn can_run_privately(task: &Task) -> bool {
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
        || frame.resume.is_some()
        || frame.tail_return.is_some()
        || frame.returning.is_some()
        || frame.after_init.is_some()
        || frame.pc >= frame.prepared.code.len()
    {
        return false;
    }
    match frame.prepared.execution[frame.pc] {
        PreparedInstruction::Constant(index) => matches!(
            frame.revision.program.constant_data[index],
            PreparedConstant::Scalar(_)
        ),
        PreparedInstruction::Local {
            index,
            store,
            rebind,
        } => match &frame.locals[index] {
            Local::Private(value) if !store => !matches!(value.get().data, Data::Uninitialized),
            Local::Private(value) if !rebind => frame.stack.last().is_some_and(|input| {
                input.typ == value.get().typ && input.logical_bytes().is_ok()
            }),
            _ => false,
        },
        PreparedInstruction::Jump { conditional, .. } => {
            !conditional || frame.stack.last().is_some_and(|value| matches!(value.data, Data::Bool(_)))
        }
        PreparedInstruction::Unary(_) => !frame.stack.is_empty(),
        PreparedInstruction::Binary(operator) => {
            frame.stack.len() >= 2
                && !(operator == crate::operators::Operator::Add
                    && matches!(
                        (&frame.stack[frame.stack.len() - 2].data, &frame.stack.last().unwrap().data),
                        (Data::String(_), Data::String(_))
                    ))
        }
        PreparedInstruction::Operand => match &frame.prepared.code[frame.pc] {
            Instruction::Pop => !frame.stack.is_empty(),
            Instruction::Zero(_) => matches!(
                frame.prepared.operand_types[frame.pc],
                Some(TypeIdentity::Any | TypeIdentity::Primitive(_) | TypeIdentity::Pointer(_) | TypeIdentity::Slice(_))
            ),
            Instruction::LoadField(payload) => frame.stack.last().is_some_and(|value| {
                matches!(&value.data, Data::Struct(fields) if fields.contains_key(&payload.field))
            }),
            Instruction::Len | Instruction::Cap => frame.stack.last().is_some_and(|value| {
                matches!(value.data, Data::String(_) | Data::Array(_) | Data::Slice(_) | Data::Nil)
            }),
            Instruction::StringRuneAt | Instruction::StringNextRuneIndex => {
                frame.stack.len() >= 2
                    && matches!(frame.stack[frame.stack.len() - 2].data, Data::String(_))
                    && frame.stack.last().is_some_and(|value| value.integer().is_ok())
            }
            Instruction::MapIterClose(_) => true,
            _ => false,
        },
        PreparedInstruction::Intrinsic(_)
        | PreparedInstruction::Upvalue { .. }
        | PreparedInstruction::Global { .. } => false,
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
    let mut steps = 0usize;
    while steps < quantum && can_run_privately(&task) {
        types.use_context(task.frames.last().unwrap().revision.program.decoded.types());
        if !step_private_instruction(&mut task, &types) {
            break;
        }
        let Some(grant) = task.step_grant.as_mut() else {
            break;
        };
        grant.consume();
        task.scheduling_phase = (task.scheduling_phase + 1) % 64;
        task.transient_roots.clear();
        steps += 1;
    }
    TaskRun { task, steps }
}

pub(super) fn step_private_instruction(task: &mut Task, types: &TypeRegistry) -> bool {
    let frame = task.frames.last().unwrap();
    let pc = frame.pc;
    let function = frame.prepared.clone();
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
            store,
            rebind,
        } => {
            if store {
                if rebind {
                    return false;
                }
                let Some(value) = frame.stack.last().cloned() else {
                    return false;
                };
                let Ok(bytes) = value.logical_bytes().and_then(|bytes| {
                    bytes.checked_add(128).ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "local", "logical size overflow")
                    })
                }) else {
                    return false;
                };
                let Local::Private(local) = &mut task.frames.last_mut().unwrap().locals[index]
                else {
                    return false;
                };
                if local.get().typ != value.typ || local.replace(value, bytes).is_err() {
                    return false;
                }
                if task.pop().is_err() {
                    return false;
                }
            } else {
                let Local::Private(value) = &task.frames.last().unwrap().locals[index] else {
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
        } => {
            let jump = if conditional {
                let Some(value) = task.frames.last().unwrap().stack.last() else {
                    return false;
                };
                let Data::Bool(value) = value.data else {
                    return false;
                };
                if task.pop().is_err() {
                    return false;
                }
                value
            } else {
                true
            };
            if jump {
                next_pc = target;
            }
        }
        PreparedInstruction::Unary(operator) => {
            let Some(value) = task.frames.last().unwrap().stack.last().cloned() else {
                return false;
            };
            let Ok(value) = crate::operators::unary(operator, value, types) else {
                return false;
            };
            if task.pop().is_err() {
                return false;
            }
            result = Some(value);
        }
        PreparedInstruction::Binary(operator) => {
            let stack = &task.frames.last().unwrap().stack;
            if stack.len() < 2 {
                return false;
            }
            let left = stack[stack.len() - 2].clone();
            let right = stack[stack.len() - 1].clone();
            if operator == crate::operators::Operator::Add
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
            if task.pop().is_err() || task.pop().is_err() {
                return false;
            }
            result = Some(value);
        }
        PreparedInstruction::Operand => match &function.code[pc] {
            Instruction::Pop => {
                if task.pop().is_err() {
                    return false;
                }
            }
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
                let Some(value) = task.frames.last().unwrap().stack.last() else {
                    return false;
                };
                let Data::Struct(fields) = &value.data else {
                    return false;
                };
                let Some(value) = fields.get(&payload.field).cloned() else {
                    return false;
                };
                if task.pop().is_err() {
                    return false;
                }
                result = Some(value);
            }
            Instruction::Len | Instruction::Cap => {
                let capacity = matches!(function.code[pc], Instruction::Cap);
                let Some(value) = task.frames.last().unwrap().stack.last() else {
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
                if task.pop().is_err() {
                    return false;
                }
                result = Some(Value::int(length));
            }
            Instruction::StringRuneAt | Instruction::StringNextRuneIndex => {
                let stack = &task.frames.last().unwrap().stack;
                if stack.len() < 2 {
                    return false;
                }
                let Data::String(bytes) = &stack[stack.len() - 2].data else {
                    return false;
                };
                let Ok(index) = usize::try_from(match stack.last().unwrap().integer() {
                    Ok(index) => index,
                    Err(_) => return false,
                }) else {
                    return false;
                };
                if index >= bytes.len() {
                    return false;
                }
                let (rune, width) = crate::value::decode_rune(&bytes[index..]);
                let next = matches!(function.code[pc], Instruction::StringNextRuneIndex);
                if task.pop().is_err() || task.pop().is_err() {
                    return false;
                }
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
                task.frames
                    .last_mut()
                    .unwrap()
                    .map_iterators
                    .remove(&payload.local);
            }
            _ => return false,
        },
        PreparedInstruction::Intrinsic(_)
        | PreparedInstruction::Upvalue { .. }
        | PreparedInstruction::Global { .. } => return false,
    }
    let frame = task.frames.last_mut().unwrap();
    frame.pc = next_pc;
    if let Some(value) = result {
        frame.stack.push(value);
    }
    true
}
