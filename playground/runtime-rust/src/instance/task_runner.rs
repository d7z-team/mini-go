//! Task-private instructions shared by the serial owner and native worker
//! slices. Shared storage, lifecycle changes and potentially blocking
//! operations use the owner's instruction transaction.

use super::{frame::Local, scheduler::Task, *};
use crate::executor_pool::{Next, Pool};
use crate::program::{Instruction, PreparedConstant, PreparedInstruction};
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
        mpsc,
    },
    task::Wake as _,
};

pub(super) const INSTRUCTION_QUANTUM: usize = 512;

pub(crate) struct TaskRun {
    pub(super) task: Task,
    pub(super) steps: usize,
}

pub(crate) struct TaskBatch {
    receiver: mpsc::Receiver<(usize, TaskRun)>,
    runs: Vec<Option<TaskRun>>,
}

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
        || task.pending_error.is_some()
        || task.instruction_active
        || task.frames.is_empty()
    {
        return false;
    }
    let frame = task.frames.last().unwrap();
    let types = frame.revision.program.decoded.types();
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
            .any(|position| !frame.input_initialized_at(frame.pc, position))
    {
        return false;
    }
    match frame.prepared.execution[frame.pc] {
        PreparedInstruction::GetPath(index) => !frame.prepared.field_paths[index]
            .iter()
            .any(|step| step.indirect),
        PreparedInstruction::Constant(index) => matches!(
            frame.revision.program.constant_data[index],
            PreparedConstant::Scalar(_)
        ),
        PreparedInstruction::Local {
            index,
            store,
            rebind,
            move_source,
        } => match &frame.locals[index] {
            Local::Private(value) if !store => {
                frame.heap.payload_initialized(*value).unwrap_or(false)
            }
            Local::Shared(value) if !store => {
                frame.heap.payload_initialized(*value).unwrap_or(false)
            }
            Local::Inline(slot) if !store => !frame.scalar_values.is_uninitialized(*slot),
            Local::Aggregate(slot) if !store => !frame.aggregate_values.is_uninitialized(*slot),
            Local::Private(value) if !rebind => {
                frame.operand_from_end(frame.pc, 0).is_some_and(|input| {
                    frame
                        .heap
                        .payload_type(*value)
                        .is_ok_and(|stored| stored.as_ref() == Some(&input.typ))
                })
            }
            Local::Inline(slot) if !rebind => {
                frame.operand_from_end(frame.pc, 0).is_some_and(|input| {
                    crate::instance::frame::InlineScalar::from_value(&input).is_some()
                        && !frame.scalar_values.is_uninitialized(*slot)
                })
            }
            Local::Aggregate(destination) if store && !rebind && move_source != u32::MAX => {
                frame.aggregate_values.can_copy_slot_from(
                    *destination,
                    &frame.slot_aggregate_values,
                    move_source as usize,
                )
            }
            _ => false,
        },
        PreparedInstruction::Jump { conditional, .. } => {
            !conditional
                || frame
                    .operand_from_end(frame.pc, 0)
                    .is_some_and(|value| matches!(value.data, Data::Bool(_)))
        }
        PreparedInstruction::Unary(_) => frame.operand_count(frame.pc) != 0,
        PreparedInstruction::MinMax { arg_count, .. } => frame.operand_count(frame.pc) >= arg_count,
        PreparedInstruction::LenCompareBranch { .. } => {
            let descriptor = frame
                .operand_count(frame.pc)
                .checked_sub(2)
                .and_then(|position| frame.input_string_descriptor(frame.pc, position))
                .is_some();
            let prepared_length = frame
                .operand_count(frame.pc)
                .checked_sub(2)
                .and_then(|position| {
                    frame
                        .input_prepared_len_cap(frame.pc, position, false, types)
                        .ok()
                        .flatten()
                })
                .is_some();
            frame.operand_count(frame.pc) >= 2
                && (descriptor
                    || prepared_length
                    || frame.operand_from_end(frame.pc, 1).is_some_and(|value| {
                        matches!(
                            value.data,
                            Data::String(_) | Data::Array(_) | Data::Aggregate(_) | Data::Slice(_)
                        )
                    }))
        }
        PreparedInstruction::Binary(operator)
        | PreparedInstruction::CompareBranch { operator, .. } => {
            frame.operand_count(frame.pc) >= 2
                && !(operator.operator == crate::operators::Operator::Add
                    && matches!(
                        (
                            &frame.operand_from_end(frame.pc, 1).unwrap().data,
                            &frame.operand_from_end(frame.pc, 0).unwrap().data
                        ),
                        (Data::String(_), Data::String(_))
                    ))
        }
        PreparedInstruction::UpdateCompareBranch { .. } => {
            frame.operand_count(frame.pc) >= 3
                && frame
                    .prepared
                    .operands
                    .inputs_at(frame.pc)
                    .first()
                    .is_some_and(|input| {
                        input.kind == 2
                            && matches!(
                                frame.locals.get(input.index as usize),
                                Some(Local::Inline(_))
                            )
                    })
        }
        PreparedInstruction::Operand => match &frame.prepared.code[frame.pc] {
            Instruction::LoadIndex | Instruction::IndexConvert(_) => {
                frame.operand_count(frame.pc) >= 2
            }
            Instruction::Pop => frame.operand_count(frame.pc) != 0,
            Instruction::Zero(_) => matches!(
                frame.prepared.operand_types[frame.pc],
                Some(
                    TypeIdentity::Any
                        | TypeIdentity::Primitive(_)
                        | TypeIdentity::Pointer(_)
                        | TypeIdentity::Slice(_)
                )
            ),
            Instruction::LoadField(_) | Instruction::Len | Instruction::Cap => {
                frame.operand_count(frame.pc) >= 1
            }
            Instruction::StringRuneAt | Instruction::StringNextRuneIndex => {
                let descriptor = frame
                    .operand_count(frame.pc)
                    .checked_sub(2)
                    .and_then(|position| frame.input_string_descriptor(frame.pc, position))
                    .is_some();
                frame.operand_count(frame.pc) >= 2
                    && (descriptor
                        || matches!(
                            frame.operand_from_end(frame.pc, 1).unwrap().data,
                            Data::String(_)
                        ))
                    && frame
                        .operand_from_end(frame.pc, 0)
                        .is_some_and(|value| value.index().is_ok())
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
            // A continuation crossing back to the owner cannot retain a
            // nursery page owned by this executor thread.  Flush before the
            // result enters the shared queue so shutdown/GC can observe a
            // committed directory with zero active worker leases.
            run.task.publish_worker_storage();
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
        let frame = task.frames.last().unwrap();
        let pc = frame.pc;
        let Some(cost) = frame.prepared.costs.get(pc) else {
            break;
        };
        let cost = (*cost).max(1) as usize;
        if remaining < cost as u64 || cost > quantum - steps {
            break;
        }
        if !try_execute_private_instruction(task, types) {
            break;
        }
        task.step_grant.as_mut().unwrap().remaining -= cost as u64;
        task.scheduling_phase = (task.scheduling_phase + cost as u32) % TASK_POLL_INTERVAL;
        task.transient_roots.clear();
        steps += cost;
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
        PreparedInstruction::LenCompareBranch { .. } => 2,
        PreparedInstruction::UpdateCompareBranch { .. } => 3,
        PreparedInstruction::Operand => match function.code[pc] {
            Instruction::Pop | Instruction::LoadField(_) | Instruction::Len | Instruction::Cap => 1,
            Instruction::LoadIndex
            | Instruction::IndexConvert(_)
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
    let mut direct_local_result = None;
    let mut local_update = None;
    match function.execution[pc] {
        PreparedInstruction::MinMax {
            kind,
            choose_min,
            arg_count,
        } => {
            if frame.operand_count(pc) < arg_count {
                return false;
            }
            let values = (0..arg_count)
                .rev()
                .map(|index| frame.operand_from_end(pc, index).unwrap());
            let values = values.collect::<Vec<_>>();
            let Ok(value) = crate::operators::min_max(
                kind,
                if choose_min { "min" } else { "max" },
                values.iter(),
            ) else {
                return false;
            };
            result = Some(value);
        }
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
                let moved_source = (move_source != u32::MAX).then_some(move_source as usize);
                let copied = moved_source.is_some_and(|source| {
                    let Local::Aggregate(destination) = frame.locals[index] else {
                        return false;
                    };
                    frame.aggregate_values.copy_slot_from(
                        destination,
                        &frame.slot_aggregate_values,
                        source,
                    )
                }) || (!rebind
                    && frame
                        .copy_prepared_input_to_aggregate_local(frame.pc, index, types)
                        .unwrap_or(false));
                if copied {
                    if let Some(source) = moved_source {
                        frame.clear_slot(source);
                    }
                } else {
                    let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                        return false;
                    };
                    let Ok(bytes) = frame.heap.logical_allocation_bytes(&value) else {
                        return false;
                    };
                    let value = if move_source == u32::MAX {
                        value
                    } else {
                        // Keep the source lane published until the destination
                        // commit succeeds. Re-encoding a dynamic value during a
                        // rejected store can itself exceed the guest budget, so
                        // taking it first would make rollback non-atomic.
                        frame.slot_value(move_source as usize).unwrap()
                    };
                    match &frame.locals[index] {
                        Local::Inline(slot) => {
                            if crate::instance::frame::InlineScalar::from_value(&value).is_none() {
                                return false;
                            }
                            frame.scalar_values.set(*slot, &value).unwrap();
                        }
                        Local::Aggregate(slot) => {
                            if frame
                                .aggregate_values
                                .set(*slot, &value, &frame.heap, types)
                                .is_err()
                            {
                                return false;
                            }
                        }
                        Local::Private(local) => {
                            if !frame
                                .heap
                                .payload_type(*local)
                                .is_ok_and(|stored| stored.as_ref() == Some(&value.typ))
                            {
                                return false;
                            }
                            if let Err((_, _value)) = frame.heap.replace(*local, value, bytes) {
                                // No owner safepoint or GC occurs inside replacement. A
                                // rejected charge leaves the source lane untouched.
                                return false;
                            }
                        }
                        Local::Shared(_) => return false,
                        Local::Uninitialized => return false,
                    }
                    if let Some(source) = moved_source {
                        frame.clear_slot(source);
                    }
                }
            } else {
                result = match &frame.locals[index] {
                    Local::Inline(slot) => {
                        let value = frame
                            .scalar_values
                            .get(*slot, &frame.prepared.local_types[index])
                            .value(&frame.prepared.local_types[index]);
                        (!matches!(value.data, Data::Uninitialized)).then_some(value)
                    }
                    Local::Aggregate(_) => {
                        // Keep the aggregate in its prepared bank until the
                        // output lane is known. The direct path below avoids
                        // decoding and re-encoding the whole local.
                        direct_local_result = Some(index);
                        None
                    }
                    Local::Private(_) | Local::Shared(_) => {
                        // The managed payload is already in the declared
                        // prepared layout. Let the output publication copy
                        // its bytes directly into the destination bank.
                        direct_local_result = Some(index);
                        None
                    }
                    Local::Uninitialized => None,
                };
                if result.is_none() && direct_local_result.is_none() {
                    return false;
                }
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
            let direct = function.field_paths[index]
                .iter()
                .all(|step| !step.indirect)
                .then(|| {
                    let path = function.field_paths[index]
                        .iter()
                        .map(|step| step.index)
                        .collect::<Vec<_>>();
                    let position = function.operands.inputs_at(pc).len().checked_sub(1)?;
                    frame
                        .input_prepared_path(pc, position, &path, types)
                        .ok()
                        .flatten()
                })
                .flatten();
            if let Some(value) = direct {
                result = Some(value);
            } else {
                let Some(mut value) = frame.operand_from_end(pc, 0) else {
                    return false;
                };
                for step in &function.field_paths[index] {
                    if step.indirect {
                        return false;
                    }
                    let Ok(field) = value.element(step.index) else {
                        return false;
                    };
                    value = field;
                }
                result = Some(value);
            }
        }
        PreparedInstruction::Unary(operator) => {
            let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                return false;
            };
            let Ok(value) = crate::operators::unary(operator, value, types) else {
                return false;
            };
            result = Some(value);
        }
        PreparedInstruction::LenCompareBranch {
            operator,
            target,
            when,
        } => {
            let Some(right) = frame.operand_from_end(pc, 0) else {
                return false;
            };
            let descriptor = function
                .operands
                .inputs_at(pc)
                .len()
                .checked_sub(2)
                .and_then(|position| frame.input_string_descriptor(pc, position));
            let prepared_length = function
                .operands
                .inputs_at(pc)
                .len()
                .checked_sub(2)
                .and_then(|position| {
                    frame
                        .input_prepared_len_cap(pc, position, false, types)
                        .ok()
                        .flatten()
                });
            let length = if let Some(descriptor) = descriptor {
                descriptor.length as usize
            } else if let Some(length) = prepared_length {
                length
            } else {
                let Some(left) = frame.operand_from_end(pc, 1) else {
                    return false;
                };
                match &left.data {
                    Data::String(bytes) => bytes.len(),
                    Data::Array(values) => values.len(),
                    Data::Aggregate(value) if value.is_array() => value.len(),
                    Data::Slice(slice) => slice.length,
                    _ => return false,
                }
            };
            let Ok(matched) =
                crate::operators::compare_length(operator.operator, length as i64, &right)
            else {
                return false;
            };
            if matched == when {
                next_pc = target;
            }
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
            let Ok(value) = crate::operators::binary(operator, &left, &right, types) else {
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
        PreparedInstruction::UpdateCompareBranch {
            update,
            comparison,
            target,
            when,
        } => {
            let (Some(current), Some(delta), Some(bound)) = (
                frame.operand_from_end(pc, 2),
                frame.operand_from_end(pc, 1),
                frame.operand_from_end(pc, 0),
            ) else {
                return false;
            };
            let Ok(updated) = crate::operators::binary(update, &current, &delta, types) else {
                return false;
            };
            let Ok(compared) = crate::operators::binary(comparison, &updated, &bound, types) else {
                return false;
            };
            let Data::Bool(matched) = compared.data else {
                return false;
            };
            let local = function.operands.inputs_at(pc)[0].index as usize;
            let Some(Local::Inline(slot)) = frame.locals.get(local) else {
                return false;
            };
            if crate::instance::frame::InlineScalar::from_value(&updated).is_none() {
                return false;
            }
            local_update = Some((*slot, updated));
            if matched == when {
                next_pc = target;
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
                let direct = frame.input_type_at(pc, 0).and_then(|typ| {
                    let Some(crate::types::StorageTypeLayout::Struct { fields }) =
                        types.storage_layout(typ).ok()?
                    else {
                        return None;
                    };
                    let index = fields.iter().position(|(name, _)| name == &payload.field)?;
                    frame
                        .input_prepared_path(pc, 0, &[index], types)
                        .ok()
                        .flatten()
                });
                if let Some(value) = direct {
                    result = Some(value);
                } else {
                    let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                        return false;
                    };
                    let Ok(value) = value.field(&payload.field) else {
                        return false;
                    };
                    result = Some(value);
                }
            }
            Instruction::LoadIndex | Instruction::IndexConvert(_) => {
                let Some(key) = frame.operand_from_end(pc, 0) else {
                    return false;
                };
                let Ok(index) = key.index() else {
                    return false;
                };
                let string_descriptor = function
                    .operands
                    .inputs_at(pc)
                    .len()
                    .checked_sub(2)
                    .and_then(|position| frame.input_string_descriptor(pc, position));
                if let Some(descriptor) = string_descriptor {
                    let Ok(Some(bytes)) = descriptor.read(&frame.heap, index, 1) else {
                        return false;
                    };
                    result = Some(Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                        data: Data::Unsigned(u64::from(bytes[0])),
                    });
                } else {
                    let direct = frame
                        .input_prepared_path(pc, 0, &[index], types)
                        .ok()
                        .flatten();
                    if direct.is_some() {
                        result = direct;
                    } else {
                        let Some(object) = frame.operand_from_end(pc, 1) else {
                            return false;
                        };
                        result = match &object.data {
                            Data::String(bytes) => bytes.get(index).map(|byte| Value {
                                typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                                data: Data::Unsigned(u64::from(*byte)),
                            }),
                            Data::Array(values) => values.get(index).cloned(),
                            Data::Aggregate(value) if value.is_array() => value.get(index).ok(),
                            _ => return false,
                        };
                    }
                    if result.is_none() {
                        return false;
                    }
                }
                if matches!(function.code[pc], Instruction::IndexConvert(_)) {
                    let Some(target) = function.operand_types[pc].as_ref() else {
                        return false;
                    };
                    let Ok(value) =
                        crate::operators::convert(result.take().unwrap(), target.clone(), types)
                    else {
                        return false;
                    };
                    result = Some(value);
                }
            }
            Instruction::Len | Instruction::Cap => {
                let capacity = matches!(function.code[pc], Instruction::Cap);
                let object_position = function.operands.inputs_at(pc).len().checked_sub(1);
                let prepared_bound = match object_position {
                    Some(position) => {
                        match frame.input_prepared_len_cap(pc, position, capacity, types) {
                            Ok(bound) => bound,
                            Err(_) => return false,
                        }
                    }
                    None => None,
                };
                if let Some(bound) = prepared_bound {
                    result = Some(Value::int(bound as i64));
                    // A prepared array/slice header supplied the complete
                    // answer; do not decode its owning aggregate below.
                } else {
                    let string_descriptor = (!capacity)
                        .then(|| {
                            function
                                .operands
                                .inputs_at(pc)
                                .len()
                                .checked_sub(1)
                                .and_then(|position| frame.input_string_descriptor(pc, position))
                        })
                        .flatten();
                    if let Some(descriptor) = string_descriptor {
                        result = Some(Value::int(i64::from(descriptor.length)));
                    } else {
                        let Some(value) = frame.operand_from_end(frame.pc, 0) else {
                            return false;
                        };
                        let pointer_array_length = match types.pointer_element(&value.typ) {
                            Ok(Some(element)) => match types.node(&element) {
                                Ok(Some((_, node))) if node.kind == wire::Array => {
                                    Some(node.length)
                                }
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
                            (Data::Aggregate(value), None) if value.is_array() => {
                                value.len() as i64
                            }
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
                }
            }
            Instruction::StringRuneAt | Instruction::StringNextRuneIndex => {
                if frame.operand_count(pc) < 2 {
                    return false;
                }
                let Ok(index) = frame.operand_from_end(pc, 0).unwrap().index() else {
                    return false;
                };
                let string_descriptor = function
                    .operands
                    .inputs_at(pc)
                    .len()
                    .checked_sub(2)
                    .and_then(|position| frame.input_string_descriptor(pc, position));
                let (rune, width) = if let Some(descriptor) = string_descriptor {
                    let Ok(Some(bytes)) = descriptor.read(&frame.heap, index, 4) else {
                        return false;
                    };
                    crate::value::decode_rune(&bytes)
                } else {
                    let Some(Value {
                        data: Data::String(bytes),
                        ..
                    }) = frame.operand_from_end(pc, 1)
                    else {
                        return false;
                    };
                    if index >= bytes.len() {
                        return false;
                    }
                    crate::value::decode_rune(&bytes[index..])
                };
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
            Instruction::MapIterClose(_) => {
                if let Some(local) = frame
                    .prepared
                    .map_iterator_indices
                    .get(frame.pc)
                    .and_then(|local| *local)
                {
                    if let Some(iterator) = frame.map_iterators.get_mut(local) {
                        *iterator = None;
                    }
                } else {
                    return false;
                }
            }
            _ => return false,
        },
        PreparedInstruction::Intrinsic(_)
        | PreparedInstruction::Construct(_)
        | PreparedInstruction::Upvalue { .. }
        | PreparedInstruction::Global { .. }
        | PreparedInstruction::TypeDispatch(_) => return false,
    }
    if let Some(value) = result.as_ref() {
        let Some(output) = frame.prepared.operands.outputs_at(pc).first().copied() else {
            return false;
        };
        let output_type = if output & crate::program::LOCAL_OUTPUT != 0 {
            frame
                .prepared
                .local_types
                .get((output & !crate::program::LOCAL_OUTPUT) as usize)
        } else {
            frame.prepared.slot_types.get(output as usize)
        };
        let Some(output_type) = output_type else {
            return false;
        };
        // Private workers cannot perform Instance::coerce. If the fast
        // result already has the declared type, the owner path can publish
        // it without a second representation conversion; otherwise leave
        // the instruction untouched for the owner to coerce atomically.
        if !types.identical(&value.typ, output_type).unwrap_or(false) {
            return false;
        }
    }
    frame.begin_slots(pc);
    frame.slot_input = 0;
    if let Some((slot, value)) = local_update
        && frame.scalar_values.set(slot, &value).is_err()
    {
        return false;
    }
    if let Some(index) = direct_local_result {
        let pushed = match frame.push_prepared_local_result(index) {
            Ok(pushed) => pushed,
            Err(_) => return false,
        };
        if !pushed {
            result = match frame.locals.get(index) {
                Some(Local::Aggregate(slot)) => frame
                    .aggregate_values
                    .get(
                        *slot,
                        &frame.prepared.local_types[index],
                        &frame.heap,
                        types,
                    )
                    .ok()
                    .flatten(),
                _ => None,
            };
            if result.is_none() {
                return false;
            }
        }
    }
    if let Some(value) = result
        && frame.push_result(&value).is_err()
    {
        return false;
    }
    frame.pc = next_pc;
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use test_helpers::micro_code;

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
                "code":micro_code(json!([integer,integer,record,record,integer]), &[
                    ("const",json!({"constant":"a"}),json!({"outputs":[0]})),
                    ("const",json!({"constant":"b"}),json!({"outputs":[1]})),
                    ("make_struct",json!({"type":record,"fields":["a","b"]}),json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]})),
                    ("store_local",json!({"local":"record"}),json!({"inputs":[[0,2]],"release":[2]})),
                    ("load_local",json!({"local":"record"}),json!({"outputs":[3]})),
                    ("load_field",json!({"field":"a"}),json!({"inputs":[[0,3]],"outputs":[4],"release":[3]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,4]],"release":[4]}))])}]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        assert_eq!(vm.poll_steps(4096).unwrap(), PollStatus::Ready);
        assert_eq!(vm.guest_results()[0].integer().unwrap(), 42);
        vm.close().unwrap();
    }

    #[test]
    fn len_and_cap_read_managed_slice_header_without_decoding_elements() {
        let program = test_helpers::program_with_artifact(|artifact| {
            let integer = json!({"kind": wire::Primitive, "primitive": wire::PrimitiveInt});
            let slice = json!({"kind": wire::Slice, "node": "slice"});
            artifact["type_table"] = json!({
                "nodes": [{"id":"slice", "kind":wire::Slice, "elem":integer.clone()}]
            });
            artifact["constants"] = json!([
                {"id":"length", "type":integer.clone(), "value":3}
            ]);
            artifact["functions"] = json!([{
                "id":"fn.Main",
                "signature":{"results":[integer.clone(),integer.clone()]},
                "locals":[{"id":"value","type":slice.clone()}],
                "code":micro_code(json!([integer.clone(),slice.clone(),integer.clone(),integer.clone()]), &[
                    ("const",json!({"constant":"length"}),json!({"outputs":[0]})),
                    ("make_slice",json!({"type":slice.clone()}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
                    // The aggregate slot is copied into the addressable local
                    // as a prepared lane; Len/Cap must then read its header.
                    ("store_local",json!({"local":"value"}),json!({"inputs":[[0,1]],"release":[1]})),
                    ("load_local",json!({"local":"value"}),json!({"outputs":[1]})),
                    ("len",json!({}),json!({"inputs":[[0,1]],"outputs":[2]})),
                    ("cap",json!({}),json!({"inputs":[[0,1]],"outputs":[3]})),
                    ("return",json!({"result_count":2}),json!({"inputs":[[0,2],[0,3]],"release":[2,3]}))
                ])
            }]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        assert_eq!(vm.poll_steps(4096).unwrap(), PollStatus::Ready);
        assert_eq!(vm.guest_results()[0].integer().unwrap(), 3);
        assert_eq!(vm.guest_results()[1].integer().unwrap(), 3);
        vm.close().unwrap();
    }

    fn division_program(divisor: i64) -> Arc<Program> {
        test_helpers::program_with_artifact(|artifact| {
            artifact["constants"] = serde_json::json!([
                {"id":"a","type":{"kind":3,"primitive":3},"value":42},
                {"id":"b","type":{"kind":3,"primitive":3},"value":divisor}]);
            artifact["functions"] = serde_json::json!([{
                "id":"fn.Main", "signature":{"results":[{"kind":3,"primitive":3}]},
                "code":micro_code(json!(vec![json!({"kind":3,"primitive":3});3]), &[
                    ("const",json!({"constant":"a"}),json!({"outputs":[0]})),
                    ("const",json!({"constant":"b"}),json!({"outputs":[1]})),
                    ("binary",json!({"operator":"/"}),json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,2]],"release":[2]}))])}]);
        })
    }

    #[test]
    fn private_slice_stops_at_its_granted_budget() {
        let mut vm = Instance::new(division_program(2), ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        vm.running.step_grant = vm.scope_steps[&vm.running.scope].reserve(2, 64).unwrap();
        let run = run_private_slice(std::mem::take(&mut vm.running), 64, vm.types.clone());
        assert_eq!(run.steps, 2);
        vm.running = run.task;
        assert_eq!(vm.running.frames.last().unwrap().pc, 2);
        let frame = vm.running.frames.last().unwrap();
        assert_eq!(
            (0..frame.prepared.slot_types.len())
                .filter(|index| frame.slot_value(*index).is_some())
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
        fast.start("default", &[]).unwrap();
        owner.start("default", &[]).unwrap();
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
                vm.running
                    .frames
                    .last()
                    .unwrap()
                    .slot_value(2)
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
                "code":micro_code(json!([byte]), &[
                    ("load_index",json!({}),json!({"inputs":[[1,0],[1,1]],"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))
                ])}]);
            });
            for private in [true, false] {
                let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
                vm.start("default", &[]).unwrap();
                if !private {
                    vm.running.begin_instruction(0);
                }
                let result = vm.poll_steps(1);
                assert_eq!(vm.steps(), 1);
                if !(0..2).contains(&index) {
                    assert_ne!(result.unwrap(), PollStatus::Ready);
                    assert!(vm.running.frames.last().unwrap().slot_value(0).is_none());
                    assert_eq!(vm.poll_steps(1).unwrap_err().code, "panic");
                    assert_eq!(vm.steps(), 1);
                } else {
                    assert_ne!(result.unwrap(), PollStatus::Ready);
                    assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
                    assert!(matches!(vm.guest_results()[0].data, Data::Unsigned(value)
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
                "code":micro_code(json!([{"kind":3,"primitive":2}]), &[
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
                max_guest_bytes: 4096,
                ..Default::default()
            },
        )
        .unwrap();
        vm.start("default", &[]).unwrap();
        assert_eq!(vm.poll_steps(3).unwrap(), PollStatus::Running);
        let before = vm.heap_stats();
        let error = vm
            .running
            .frames
            .last_mut()
            .unwrap()
            .put_slot_value(0, Value::string("x".repeat(8192)))
            .unwrap_err();
        assert_eq!(error.code, "collection_required");
        let frame = vm.running.frames.last().unwrap();
        assert_eq!(frame.pc, 3);
        assert!(frame.slot_value(0).is_some_and(|value| matches!(
            value.data,
            Data::String(bytes) if bytes.is_empty()
        )));
        assert!(matches!(
            frame.local_value(0).unwrap().data,
            Data::String(bytes) if bytes.is_empty()
        ));
        assert_eq!(vm.heap_stats().live_bytes, before.live_bytes);
        assert_eq!(
            vm.heap_stats().total_allocated_bytes,
            before.total_allocated_bytes
        );
        vm.running
            .frames
            .last_mut()
            .unwrap()
            .put_slot_value(0, Value::string("ok"))
            .unwrap();
        assert!(try_execute_private_instruction(&mut vm.running, &vm.types));
        assert_eq!(vm.running.frames.last().unwrap().pc, 4);
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
                    serde_json::json!([opcode, descriptor, operands, 1])
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
                    max_guest_bytes: 4096,
                    ..Default::default()
                },
            )
            .unwrap();
            vm.start("default", &[]).unwrap();
            assert_eq!(vm.poll_steps(3).unwrap(), PollStatus::Running);
            let before = vm.heap_stats();
            let error = vm
                .running
                .frames
                .last_mut()
                .unwrap()
                .put_slot_value(0, Value::string("x".repeat(8192)))
                .unwrap_err();
            assert_eq!(error.code, "collection_required");
            let frame = vm.running.frames.last().unwrap();
            assert_eq!(frame.pc, 3);
            assert!(frame.slot_value(0).is_some_and(|value| matches!(
                value.data,
                Data::String(bytes) if bytes.is_empty()
            )));
            assert!(matches!(
                frame.local_value(0).unwrap().data,
                Data::String(bytes) if bytes.is_empty()
            ));
            assert_eq!(vm.heap_stats().live_bytes, before.live_bytes);
            assert_eq!(
                vm.heap_stats().total_allocated_bytes,
                before.total_allocated_bytes
            );
            vm.running
                .frames
                .last_mut()
                .unwrap()
                .put_slot_value(0, Value::string("ok"))
                .unwrap();
            assert!(try_execute_private_instruction(&mut vm.running, &vm.types));
            let frame = vm.running.frames.last().unwrap();
            assert_eq!(frame.pc, 4);
            assert_eq!(frame.slot_value(0).is_none(), release);
            assert!(matches!(
                frame.local_value(0).unwrap().data,
                Data::String(bytes) if bytes.as_ref() == b"ok"
            ));
            vm.close().unwrap();
        }
    }

    #[test]
    fn private_binary_failure_preserves_inputs_and_success_preserves_buffer_charge() {
        for divisor in [0, 2] {
            let mut vm =
                Instance::new(division_program(divisor), ExecutionLimits::default()).unwrap();
            vm.start("default", &[]).unwrap();
            vm.poll_steps(2).unwrap();
            assert_eq!(
                try_execute_private_instruction(&mut vm.running, &vm.types),
                divisor != 0
            );
            let frame = vm.running.frames.last().unwrap();
            if divisor == 0 {
                assert_eq!(frame.pc, 2);
                assert_eq!(frame.slot_value(0).unwrap().integer().unwrap(), 42);
                assert_eq!(frame.slot_value(1).unwrap().integer().unwrap(), 0);
            } else {
                assert_eq!(frame.pc, 3);
                assert_eq!(frame.slot_value(2).unwrap().integer().unwrap(), 21);
                assert_eq!(frame.memory.popped, 0);
            }
            vm.close().unwrap();
        }
    }
}
