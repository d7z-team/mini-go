//! Source-aware stops and detached inspection under the same machine owner.

use super::*;
use std::{
    collections::{BTreeMap, BTreeSet},
    sync::atomic::{AtomicBool, Ordering},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StepMode {
    Continue,
    Instruction,
    Into,
    Over,
    Out,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize)]
pub enum EventKind {
    Pause,
    Breakpoint,
    Step,
    Panic,
}
#[derive(Clone, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct FrameRef {
    pub epoch: u64,
    pub task: u64,
    pub depth: usize,
}
#[derive(Clone, Debug, serde::Serialize)]
pub struct FrameInfo {
    pub generation: u64,
    pub reference: FrameRef,
    pub scope: u64,
    pub program_hash: String,
    pub module: String,
    pub function: String,
    pub pc: usize,
    pub locations: Vec<wire::Location>,
}
#[derive(Clone, Debug, serde::Serialize)]
pub struct DebugEvent {
    pub sequence: u64,
    pub kind: EventKind,
    pub frame: FrameInfo,
}

#[derive(Clone, Debug, serde::Serialize)]
pub struct ThreadInfo {
    pub id: u64,
    pub state: &'static str,
    pub frames: usize,
}
#[derive(Clone, Debug)]
pub struct Bindings {
    pub names: Vec<String>,
    pub values: crate::snapshot::HostSnapshot,
}

#[derive(Clone, Debug)]
pub struct VariableRef {
    pub(super) epoch: u64,
    pub(super) root: VariableRoot,
    pub(super) path: Vec<VariablePath>,
}

#[derive(Clone, Debug)]
pub(super) enum VariableRoot {
    Address(Address),
    Local { frame: FrameRef, index: usize },
}

#[derive(Clone, Debug)]
pub(super) enum VariablePath {
    Field(String),
    Index(usize),
    MapKey(usize),
    MapValue(usize),
    Dereference,
}

#[derive(Clone, Debug)]
pub struct VariableInfo {
    pub name: String,
    pub typ: TypeIdentity,
    pub summary: String,
    pub children: usize,
    pub reference: Option<VariableRef>,
}
#[derive(Clone, Debug)]
pub struct ProfileSample {
    pub scope: u64,
    pub generation: u64,
    pub program_hash: String,
    pub module: String,
    pub function: String,
    pub pc: usize,
    pub opcode: String,
    pub locations: Vec<wire::Location>,
    pub count: u64,
}
#[derive(Clone, Debug, Default)]
pub struct Profile {
    pub sample_every: u64,
    pub dropped: u64,
    pub samples: Vec<ProfileSample>,
}

pub(crate) struct DebugState {
    pub pause: Arc<AtomicBool>,
    pub paused: bool,
    break_on_panic: bool,
    pub(super) epoch: u64,
    pub(super) breakpoints: BTreeMap<(String, String), (u64, BTreeSet<i64>)>,
    breakpoint_pcs: HashSet<(usize, usize)>,
    events: VecDeque<DebugEvent>,
    event_sequence: u64,
    pub(super) resume: Option<(StepMode, FrameInfo)>,
    resumed_instructions: u64,
    pub(super) profile_every: u64,
    pub(super) profile_phase: u64,
    profile_limit: usize,
    profile_dropped: u64,
    pub(super) scope_profile_dropped: BTreeMap<u64, u64>,
    samples: BTreeMap<(u64, u64, String, String, usize), ProfileSample>,
}

impl Default for DebugState {
    fn default() -> Self {
        Self {
            pause: Arc::new(AtomicBool::new(false)),
            paused: false,
            break_on_panic: false,
            epoch: 1,
            breakpoints: BTreeMap::new(),
            breakpoint_pcs: HashSet::new(),
            events: VecDeque::new(),
            event_sequence: 0,
            resume: None,
            resumed_instructions: 0,
            profile_every: 0,
            profile_phase: 0,
            profile_limit: 0,
            profile_dropped: 0,
            scope_profile_dropped: BTreeMap::new(),
            samples: BTreeMap::new(),
        }
    }
}

impl Instance {
    pub fn set_break_on_panic(&mut self, enabled: bool) -> Result<(), RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("closed", "debug", "instance is closed"));
        }
        self.debug.break_on_panic = enabled;
        Ok(())
    }

    pub(super) fn debug_panic(&mut self) {
        if !self.debug.break_on_panic {
            return;
        }
        let frame = self.running.frames.last().unwrap();
        let info = self.frame_info(
            self.running.id,
            self.running.scope,
            self.running.frames.len() - 1,
            frame,
            frame.pc.saturating_sub(1),
        );
        self.debug.paused = true;
        self.debug.resume = None;
        if self.debug.events.len() == 256 {
            self.debug.events.pop_front();
        }
        self.debug.event_sequence = self.debug.event_sequence.saturating_add(1);
        self.debug.events.push_back(DebugEvent {
            sequence: self.debug.event_sequence,
            kind: EventKind::Panic,
            frame: info,
        });
    }

    pub(super) fn debug_revision_changed(&mut self) {
        self.debug.epoch = self.debug.epoch.saturating_add(1);
        for (generation, _) in self.debug.breakpoints.values_mut() {
            *generation = self.revision.generation;
        }
        self.index_breakpoints();
    }

    fn index_breakpoints(&mut self) {
        self.debug.breakpoint_pcs.clear();
        for function in &self.revision.program.function_table {
            for (pc, locations) in function.locations.iter().enumerate() {
                if locations.iter().any(|point| {
                    self.debug
                        .breakpoints
                        .get(&(function.module.to_string(), point.file.clone()))
                        .is_some_and(|(_, lines)| lines.contains(&point.line))
                }) {
                    self.debug.breakpoint_pcs.insert((function.index, pc));
                }
            }
        }
    }
    pub(super) fn debug_cancel_scope(&mut self, scope: u64) {
        if self.running.scope == scope {
            self.debug.paused = false;
            self.debug.resume = None;
            self.debug.epoch = self.debug.epoch.saturating_add(1);
        }
    }
    pub fn request_pause(&self) {
        self.debug.pause.store(true, Ordering::Release);
        self.wake.signal();
    }

    pub fn set_breakpoints(
        &mut self,
        module: &str,
        file: &str,
        lines: &[i64],
    ) -> Result<Vec<i64>, RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("closed", "debug", "instance is closed"));
        }
        let package = self
            .revision
            .program
            .symbols()
            .and_then(|symbols| symbols.packages.as_ref())
            .and_then(|packages| packages.get(module))
            .ok_or_else(|| {
                RuntimeError::new(
                    "missing_symbols",
                    "breakpoints",
                    "module has no source symbols",
                )
            })?;
        let source = package
            .files
            .iter()
            .find(|source| source.id == file || source.path == file)
            .ok_or_else(|| {
                RuntimeError::new(
                    "missing_source",
                    "breakpoints",
                    "source file is unavailable",
                )
            })?;
        if lines.len() > self.limits.max_sequence_elements || lines.iter().any(|line| *line <= 0) {
            return Err(RuntimeError::new(
                "debug_limit",
                "breakpoints",
                "invalid breakpoint lines",
            ));
        }
        let requested: BTreeSet<_> = lines.iter().copied().collect();
        let active: BTreeSet<_> = package
            .functions
            .iter()
            .flat_map(|function| function.locations.iter())
            .flat_map(|location| location.points.iter())
            .filter(|point| {
                (point.file == source.id || point.file == source.path)
                    && requested.contains(&point.line)
            })
            .map(|point| point.line)
            .collect();
        self.debug
            .breakpoints
            .remove(&(module.to_owned(), source.id.clone()));
        self.debug
            .breakpoints
            .remove(&(module.to_owned(), source.path.clone()));
        if !requested.is_empty() {
            self.debug.breakpoints.insert(
                (module.to_owned(), source.id.clone()),
                (self.revision.generation, requested.clone()),
            );
            self.debug.breakpoints.insert(
                (module.to_owned(), source.path.clone()),
                (self.revision.generation, requested),
            );
        }
        self.index_breakpoints();
        Ok(active.into_iter().collect())
    }

    fn frame_info(
        &self,
        task: u64,
        scope: u64,
        depth: usize,
        frame: &Frame,
        pc: usize,
    ) -> FrameInfo {
        let locations = frame
            .prepared
            .locations
            .get(pc)
            .cloned()
            .unwrap_or_default();
        FrameInfo {
            generation: frame.revision.generation,
            reference: FrameRef {
                epoch: self.debug.epoch,
                task,
                depth,
            },
            scope,
            program_hash: frame.revision.program.image().hash.clone(),
            module: frame.module.to_string(),
            function: frame.function.to_string(),
            pc,
            locations,
        }
    }

    pub(super) fn debug_before_instruction(&mut self) -> bool {
        if self.debug.paused {
            return true;
        }
        if self.debug.resumed_instructions != 0
            && matches!(self.debug.resume, Some((StepMode::Continue, _)))
        {
            self.debug.resume = None;
        }
        let sample = self.debug.profile_every != 0 && self.debug.profile_phase == 0;
        let frame = self.running.frames.last().unwrap();
        let breakpoint = frame.revision.generation == self.revision.generation
            && self
                .debug
                .breakpoint_pcs
                .contains(&(frame.prepared.index, frame.pc));
        if !self.debug.pause.load(Ordering::Acquire)
            && !breakpoint
            && self.debug.resume.is_none()
            && !sample
        {
            return false;
        }
        let info = self.frame_info(
            self.running.id,
            self.running.scope,
            self.running.frames.len() - 1,
            frame,
            frame.pc,
        );
        let mut kind = None;
        if self.debug.pause.swap(false, Ordering::AcqRel) {
            kind = Some(EventKind::Pause);
        }
        if let Some((mode, previous)) = &self.debug.resume {
            let progressed = self.debug.resumed_instructions != 0;
            let same_task = previous.reference.task == info.reference.task;
            let mapped = !info.locations.is_empty() || frame.revision.program.symbols().is_none();
            let stepped = progressed
                && same_task
                && match mode {
                    StepMode::Continue => false,
                    StepMode::Instruction => true,
                    StepMode::Into => mapped,
                    StepMode::Over => mapped && info.reference.depth <= previous.reference.depth,
                    StepMode::Out => mapped && info.reference.depth < previous.reference.depth,
                };
            if stepped {
                kind = Some(EventKind::Step);
            }
        }
        let skip = self.debug.resume.as_ref().is_some_and(|(_, previous)| {
            self.debug.resumed_instructions == 0
                && previous.reference.task == info.reference.task
                && previous.module == info.module
                && previous.function == info.function
                && previous.pc == info.pc
        });
        if !skip && breakpoint {
            kind = Some(EventKind::Breakpoint);
        }
        if let Some(kind) = kind {
            self.debug.paused = true;
            self.debug.resume = None;
            if self.debug.events.len() == 256 {
                self.debug.events.pop_front();
            }
            self.debug.event_sequence = self.debug.event_sequence.saturating_add(1);
            self.debug.events.push_back(DebugEvent {
                sequence: self.debug.event_sequence,
                kind,
                frame: info,
            });
            return true;
        }
        if self
            .debug
            .resume
            .as_ref()
            .is_some_and(|(_, previous)| previous.reference.task == self.running.id)
        {
            self.debug.resumed_instructions = self.debug.resumed_instructions.saturating_add(1);
        }
        if sample {
            let key = (
                info.scope,
                info.generation,
                info.module.clone(),
                info.function.clone(),
                info.pc,
            );
            if let Some(sample) = self.debug.samples.get_mut(&key) {
                sample.count += 1;
            } else if self.debug.samples.len() >= self.debug.profile_limit {
                self.debug.profile_dropped += 1;
                *self
                    .debug
                    .scope_profile_dropped
                    .entry(info.scope)
                    .or_default() += 1;
            } else {
                let opcode = frame
                    .revision
                    .program
                    .function(&info.module, &info.function)
                    .unwrap()
                    .opcodes[info.pc]
                    .clone();
                self.debug.samples.insert(
                    key,
                    ProfileSample {
                        scope: info.scope,
                        generation: info.generation,
                        program_hash: info.program_hash,
                        module: info.module,
                        function: info.function,
                        pc: info.pc,
                        opcode,
                        locations: info.locations,
                        count: 1,
                    },
                );
            }
        }
        false
    }

    pub fn debug_events(&self) -> Vec<DebugEvent> {
        self.debug.events.iter().cloned().collect()
    }

    pub fn debug_threads(&self) -> Vec<ThreadInfo> {
        let mut result = Vec::new();
        if !self.running.frames.is_empty() {
            result.push(ThreadInfo {
                id: self.running.id,
                state: if self.debug.paused {
                    "paused"
                } else {
                    "running"
                },
                frames: self.running.frames.len(),
            });
        }
        for task in &self.runnable {
            result.push(ThreadInfo {
                id: task.id,
                state: "runnable",
                frames: task.frames.len(),
            });
        }
        for task in self.blocked.iter() {
            result.push(ThreadInfo {
                id: task.id,
                state: "waiting",
                frames: task.frames.len(),
            });
        }
        result.sort_by_key(|thread| thread.id);
        result
    }

    pub fn debug_stack(&self) -> Result<Vec<FrameInfo>, RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("closed", "debug", "instance is closed"));
        }
        if !self.debug.paused {
            return Err(RuntimeError::new(
                "not_paused",
                "debug",
                "inspection requires a paused owner",
            ));
        }
        let mut frames = Vec::new();
        for (depth, frame) in self.running.frames.iter().enumerate().rev() {
            frames.push(self.frame_info(
                self.running.id,
                self.running.scope,
                depth,
                frame,
                frame.pc,
            ));
        }
        for task in self.runnable.iter().chain(self.blocked.iter()) {
            for (depth, frame) in task.frames.iter().enumerate().rev() {
                frames.push(self.frame_info(task.id, task.scope, depth, frame, frame.pc));
            }
        }
        Ok(frames)
    }

    pub fn debug_bindings(
        &self,
        reference: &FrameRef,
        limits: crate::snapshot::SnapshotLimits,
    ) -> Result<Bindings, RuntimeError> {
        let bindings = self.debug_binding_roots(reference)?;
        let values = bindings
            .iter()
            .map(|(_, root)| self.debug_root_value(root).map(|value| value.into_owned()))
            .collect::<Result<Vec<_>, _>>()?;
        Ok(Bindings {
            names: bindings.into_iter().map(|(name, _)| name).collect(),
            values: crate::snapshot::HostSnapshot::capture(
                &self.heap,
                &self.types,
                &values,
                limits,
            )?,
        })
    }

    pub(super) fn debug_frame(&self, reference: &FrameRef) -> Result<&Frame, RuntimeError> {
        if !self.debug.paused || reference.epoch != self.debug.epoch {
            return Err(RuntimeError::new(
                "stale_reference",
                "debug",
                "inspection epoch has expired",
            ));
        }
        let frames = if reference.task == self.running.id {
            Some(&self.running.frames)
        } else {
            self.runnable
                .iter()
                .chain(self.blocked.iter())
                .find(|task| task.id == reference.task)
                .map(|task| &task.frames)
        };
        frames
            .and_then(|frames| frames.get(reference.depth))
            .ok_or_else(|| RuntimeError::new("stale_reference", "debug", "frame no longer exists"))
    }

    pub(super) fn debug_root_value(
        &self,
        root: &VariableRoot,
    ) -> Result<crate::value::ValueRead<'_>, RuntimeError> {
        match root {
            VariableRoot::Address(address) => self.snapshot_address(address),
            VariableRoot::Local { frame, index } => {
                let value = self.debug_frame(frame)?.locals[*index].value(&self.heap)?;
                if matches!(value.data, Data::Uninitialized) {
                    Ok(crate::value::ValueRead::Owned(self.zero(&value.typ, 0)?))
                } else {
                    Ok(value)
                }
            }
        }
    }

    pub(super) fn debug_binding_roots(
        &self,
        reference: &FrameRef,
    ) -> Result<Vec<(String, VariableRoot)>, RuntimeError> {
        let frame = self.debug_frame(reference)?;
        let function = &frame.prepared;
        let symbols = frame
            .revision
            .program
            .symbols()
            .and_then(|symbols| symbols.packages.as_ref())
            .and_then(|packages| packages.get(frame.module.as_ref()));
        let function_symbols = symbols.and_then(|package| {
            package
                .functions
                .iter()
                .find(|function| function.id == frame.function.as_ref())
        });
        let mut names = Vec::new();
        let mut addresses = Vec::new();
        for (index, local) in function.declaration.locals.iter().enumerate() {
            let symbol = function_symbols
                .and_then(|function| function.locals.iter().find(|symbol| symbol.id == local.id));
            if let Some(symbol) = symbol
                && symbol.scope != 0
                && !function_symbols.unwrap().scopes.iter().any(|scope| {
                    scope.id == symbol.scope
                        && scope.ranges.iter().any(|range| {
                            range.start <= frame.pc as i64 && (frame.pc as i64) < range.end
                        })
                })
            {
                continue;
            }
            names.push(
                symbol
                    .filter(|symbol| !symbol.name.is_empty())
                    .map_or_else(|| local.id.clone(), |symbol| symbol.name.clone()),
            );
            addresses.push(VariableRoot::Local {
                frame: reference.clone(),
                index,
            });
        }
        for (index, upvalue) in function.declaration.upvalues.iter().enumerate() {
            names.push(
                function_symbols
                    .and_then(|function| {
                        function
                            .upvalues
                            .iter()
                            .find(|symbol| symbol.id == upvalue.id)
                    })
                    .filter(|symbol| !symbol.name.is_empty())
                    .map_or_else(|| upvalue.id.clone(), |symbol| symbol.name.clone()),
            );
            addresses.push(VariableRoot::Address(frame.upvalues[index].clone()));
        }
        for ((module, id), handle) in &self.globals {
            if module.as_str() != frame.module.as_ref() {
                continue;
            }
            names.push(
                symbols
                    .and_then(|package| package.globals.iter().find(|symbol| symbol.id == *id))
                    .map_or_else(|| id.clone(), |symbol| symbol.name.clone()),
            );
            addresses.push(VariableRoot::Address(Address {
                identity: std::sync::Arc::default(),
                root: *handle,
                path: Vec::new(),
            }));
        }
        Ok(names.into_iter().zip(addresses).collect())
    }

    pub fn debug_resume(&mut self, mode: StepMode) -> Result<(), RuntimeError> {
        self.debug_resume_task(mode, self.running.id)
    }

    pub fn debug_resume_task(&mut self, mode: StepMode, task: u64) -> Result<(), RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("closed", "debug", "instance is closed"));
        }
        if !self.debug.paused {
            return Err(RuntimeError::new(
                "not_paused",
                "debug",
                "instance is not paused",
            ));
        }
        let info = self
            .debug_stack()?
            .into_iter()
            .find(|frame| frame.reference.task == task)
            .ok_or_else(|| {
                RuntimeError::new("invalid_task", "debug", "selected task has no live frame")
            })?;
        self.debug.epoch = self.debug.epoch.checked_add(1).ok_or_else(|| {
            RuntimeError::new("debug_limit", "debug", "inspection epochs exhausted")
        })?;
        self.debug.resume = Some((mode, info));
        self.debug.resumed_instructions = 0;
        self.debug.paused = false;
        self.wake.signal();
        Ok(())
    }

    pub fn start_profile(
        &mut self,
        sample_every: u64,
        max_samples: usize,
    ) -> Result<(), RuntimeError> {
        if max_samples > self.limits.max_sequence_elements {
            return Err(RuntimeError::new(
                "debug_limit",
                "profile",
                "sample capacity exceeds limit",
            ));
        }
        self.debug.profile_every = sample_every;
        self.debug.profile_phase = if sample_every == 0 {
            0
        } else {
            self.steps % sample_every
        };
        self.debug.profile_limit = max_samples;
        self.debug.profile_dropped = 0;
        self.debug.scope_profile_dropped.clear();
        self.debug.samples.clear();
        Ok(())
    }

    pub fn profile(&self) -> Profile {
        let mut samples: Vec<_> = self.debug.samples.values().cloned().collect();
        samples.sort_by(|left, right| {
            right.count.cmp(&left.count).then_with(|| {
                (&left.module, &left.function, left.pc).cmp(&(
                    &right.module,
                    &right.function,
                    right.pc,
                ))
            })
        });
        Profile {
            sample_every: self.debug.profile_every,
            dropped: self.debug.profile_dropped,
            samples,
        }
    }

    pub(crate) fn profile_for_scope(&self, scope: u64) -> Profile {
        Profile {
            sample_every: self.debug.profile_every,
            dropped: self
                .debug
                .scope_profile_dropped
                .get(&scope)
                .copied()
                .unwrap_or_default(),
            samples: self
                .debug
                .samples
                .values()
                .filter(|sample| sample.scope == scope)
                .cloned()
                .collect(),
        }
    }
}
