//! Provider selection, registration generations, and draining leases.
mod mount;
pub use super::catalog::{ContractReference, ContractRepository, MrpcBundle, MrpcFile};
use super::platform::Handle;
use super::*;
use crate::ffi::Cancellation;
pub use mount::MountedProvider;
use std::collections::BTreeMap;
use std::sync::{
    Arc, Mutex, Weak,
    atomic::{AtomicBool, Ordering},
};
use tokio::sync::{Semaphore, watch};

#[derive(Clone, Debug)]
pub struct RegistrationOptions {
    pub name: String,
    pub priority: i64,
    pub weight: u64,
    pub max_leases: usize,
    pub labels: BTreeMap<String, String>,
}
impl Default for RegistrationOptions {
    fn default() -> Self {
        Self {
            name: String::new(),
            priority: 0,
            weight: 1,
            max_leases: 0,
            labels: BTreeMap::new(),
        }
    }
}
pub type Authorizer =
    Arc<dyn Fn(CallContext, PeerInfo, Contract) -> BoxFuture<'static, Result<()>> + Send + Sync>;
pub type OwnedClose = Box<dyn FnOnce() -> BoxFuture<'static, Result<()>> + Send>;

struct State {
    closed: bool,
    next_id: u64,
    epoch: u64,
    registrations: BTreeMap<u64, Arc<Registration>>,
    owned: BTreeMap<u64, Arc<Registration>>,
    round_robin: BTreeMap<String, u64>,
}
impl State {
    fn revision(&self, router_id: &str) -> Revision {
        Revision {
            router_id: router_id.into(),
            route_epoch: self.epoch,
            registrations: self.registrations.keys().copied().collect(),
            references: self
                .registrations
                .values()
                .flat_map(|entry| entry.references.iter().cloned())
                .collect::<std::collections::BTreeSet<_>>()
                .into_iter()
                .collect(),
        }
    }
    fn prune_selection_counters(&mut self) {
        self.round_robin.retain(|service, _| {
            self.registrations.values().any(|entry| {
                entry
                    .contract
                    .methods
                    .iter()
                    .any(|method| method.service == *service)
            })
        });
    }
}
struct Owner {
    id: String,
    runtime: Handle,
    limits: Limits,
    state: Mutex<State>,
    changed: watch::Sender<u64>,
    authorizer: Option<Authorizer>,
    repository: ContractRepository,
    done: watch::Sender<Option<Result<()>>>,
}
pub struct Router {
    owner: Arc<Owner>,
}
impl catalog::CatalogSource for Router {
    fn snapshot(&self) -> Result<catalog::Snapshot> {
        let revision = self.revision();
        let mut snapshot = catalog::Snapshot::new(revision.references)?;
        snapshot.gateway_id = revision.router_id;
        snapshot.route_epoch = revision.route_epoch;
        Ok(snapshot)
    }
    fn watch(
        &self,
        context: CallContext,
        after: catalog::Snapshot,
    ) -> BoxFuture<'_, Result<catalog::Snapshot>> {
        Box::pin(async move {
            let revision =
                Router::watch(self, context, &after.gateway_id, after.route_epoch).await?;
            let mut snapshot = catalog::Snapshot::new(revision.references)?;
            snapshot.gateway_id = revision.router_id;
            snapshot.route_epoch = revision.route_epoch;
            Ok(snapshot)
        })
    }
    fn resolve(&self, reference: &ContractReference) -> Result<MrpcBundle> {
        self.resolve_contract(reference)
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Revision {
    pub router_id: String,
    pub route_epoch: u64,
    pub registrations: Vec<u64>,
    pub references: Vec<ContractReference>,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Lifecycle {
    Open,
    Draining,
    Closed,
}
#[derive(Clone, Debug)]
pub struct RegistrationStatus {
    pub id: u64,
    pub name: String,
    pub state: Lifecycle,
    pub healthy: bool,
    pub active_leases: usize,
    pub retired: bool,
}
#[derive(Clone, Debug)]
pub struct RouterStatus {
    pub router_id: String,
    pub route_epoch: u64,
    pub state: Lifecycle,
    pub registrations: Vec<RegistrationStatus>,
}

impl Router {
    pub fn new(runtime: Handle, limits: Limits, authorizer: Option<Authorizer>) -> Self {
        let (changed, _) = watch::channel(0);
        let mut identity = [0u8; 16];
        getrandom::fill(&mut identity).expect("RPC Router requires OS randomness");
        Self {
            owner: Arc::new(Owner {
                id: identity.iter().map(|byte| format!("{byte:02x}")).collect(),
                runtime,
                limits,
                authorizer,
                changed,
                repository: ContractRepository::default(),
                done: watch::channel(None).0,
                state: Mutex::new(State {
                    closed: false,
                    next_id: 1,
                    epoch: 0,
                    registrations: BTreeMap::new(),
                    owned: BTreeMap::new(),
                    round_robin: BTreeMap::new(),
                }),
            }),
        }
    }
    pub fn register(
        &self,
        provider: Arc<dyn Provider>,
        options: RegistrationOptions,
    ) -> Result<Arc<Registration>> {
        self.register_owned(provider, options, None)
    }
    pub fn register_owned(
        &self,
        provider: Arc<dyn Provider>,
        options: RegistrationOptions,
        close: Option<OwnedClose>,
    ) -> Result<Arc<Registration>> {
        let prepared = PreparedRegistration::new(&self.owner.limits, provider, options, close)?;
        let mut state = self.owner.state.lock().unwrap();
        if state.closed {
            return Err(Status::new("unavailable", "RPC Router closed"));
        }
        if state.owned.len() >= self.owner.limits.max_bindings {
            return Err(Status::exhausted("RPC registration limit exceeded"));
        }
        let id = state.next_id;
        state.next_id = id
            .checked_add(1)
            .ok_or_else(|| Status::exhausted("RPC registration IDs exhausted"))?;
        let registration = prepared.install(&self.owner, id);
        state.registrations.insert(id, registration.clone());
        state.owned.insert(id, registration.clone());
        state.epoch += 1;
        self.owner.changed.send_replace(state.epoch);
        Ok(registration)
    }
    pub fn revision(&self) -> Revision {
        let state = self.owner.state.lock().unwrap();
        state.revision(&self.owner.id)
    }
    pub fn status(&self) -> RouterStatus {
        let state = self.owner.state.lock().unwrap();
        RouterStatus {
            router_id: self.owner.id.clone(),
            route_epoch: state.epoch,
            state: if self.owner.done.borrow().is_some() {
                Lifecycle::Closed
            } else if state.closed {
                Lifecycle::Draining
            } else {
                Lifecycle::Open
            },
            registrations: state.owned.values().map(|entry| entry.status()).collect(),
        }
    }
    pub fn resolve_contract(&self, reference: &ContractReference) -> Result<MrpcBundle> {
        self.owner.repository.resolve(reference)
    }
    pub async fn watch(
        &self,
        context: CallContext,
        router_id: &str,
        epoch: u64,
    ) -> Result<Revision> {
        let mut changed = self.owner.changed.subscribe();
        context
            .run(async {
                loop {
                    changed.borrow_and_update();
                    let revision = {
                        let state = self.owner.state.lock().unwrap();
                        if state.closed {
                            return Err(Status::new("unavailable", "RPC Router closed"));
                        }
                        state.revision(&self.owner.id)
                    };
                    if revision.router_id != router_id || revision.route_epoch != epoch {
                        return Ok(revision);
                    }
                    changed
                        .changed()
                        .await
                        .map_err(|_| Status::new("unavailable", "RPC Router closed"))?;
                }
            })
            .await
    }
    pub fn begin_shutdown(&self) {
        let registrations = {
            let mut state = self.owner.state.lock().unwrap();
            if state.closed {
                return;
            }
            state.closed = true;
            state.registrations.clear();
            state.round_robin.clear();
            state.epoch += 1;
            self.owner.changed.send_replace(state.epoch);
            state.owned.values().cloned().collect::<Vec<_>>()
        };
        for registration in &registrations {
            registration.drain();
        }
        let owner = self.owner.clone();
        self.owner.runtime.spawn(async move {
            let mut result = Ok(());
            for registration in registrations {
                if let Err(error) = registration.wait_closed().await {
                    result = Err(error);
                }
            }
            owner.done.send_replace(Some(result));
        });
    }
    pub async fn shutdown(&self) -> Result<()> {
        self.begin_shutdown();
        let mut done = self.owner.done.subscribe();
        let result = done
            .wait_for(|value| value.is_some())
            .await
            .map_err(|_| Status::new("internal", "Router shutdown owner failed"))?;
        result.as_ref().unwrap().clone()
    }
    pub async fn force_shutdown(&self) -> Result<()> {
        self.begin_shutdown();
        let registrations = self
            .owner
            .state
            .lock()
            .unwrap()
            .owned
            .values()
            .cloned()
            .collect::<Vec<_>>();
        for registration in registrations {
            registration.abort();
        }
        self.shutdown().await
    }
}

struct PreparedRegistration {
    provider: Arc<dyn Provider>,
    options: RegistrationOptions,
    contract: Contract,
    capacity: usize,
    close: Option<OwnedClose>,
    references: Vec<ContractReference>,
}
impl PreparedRegistration {
    fn new(
        limits: &Limits,
        provider: Arc<dyn Provider>,
        mut options: RegistrationOptions,
        close: Option<OwnedClose>,
    ) -> Result<Self> {
        let contract = provider.contract().normalized(limits)?;
        if options.weight == 0 {
            options.weight = 1;
        }
        let capacity = if options.max_leases == 0 {
            limits.max_bindings
        } else {
            options.max_leases
        };
        if capacity == 0 || capacity > u32::MAX as usize {
            return Err(Status::new("invalid_argument", "invalid provider capacity"));
        }
        Ok(Self {
            provider,
            options,
            contract,
            capacity,
            close,
            references: Vec::new(),
        })
    }
    fn install(self, owner: &Arc<Owner>, id: u64) -> Arc<Registration> {
        let (done, receiver) = watch::channel(None);
        let registration = Arc::new(Registration {
            owner: Arc::downgrade(owner),
            id,
            provider: self.provider,
            contract: self.contract,
            options: self.options,
            references: self.references,
            healthy: AtomicBool::new(true),
            draining: Cancellation::default(),
            aborted: Cancellation::default(),
            admission: Mutex::new(()),
            leases: Arc::new(Semaphore::new(self.capacity)),
            capacity: self.capacity,
            done: receiver,
        });
        let entry = registration.clone();
        owner.runtime.spawn(async move {
            entry.draining.cancelled().await;
            let _leases = entry.leases.acquire_many(entry.capacity as u32).await;
            let result = if let Some(close) = self.close {
                super::binding::invoke_user(async { close().await }).await
            } else {
                Ok(())
            };
            done.send_replace(Some(result));
            if let Some(owner) = entry.owner.upgrade() {
                owner.state.lock().unwrap().owned.remove(&entry.id);
            }
        });
        registration
    }
}

pub struct ProviderEntry {
    pub provider: Arc<dyn Provider>,
    pub options: RegistrationOptions,
    pub bundles: Vec<MrpcBundle>,
}
pub struct Publication {
    router_id: String,
    owner: Weak<Owner>,
    entries: Vec<Arc<Registration>>,
}
#[derive(Clone, Debug)]
pub struct PublicationStatus {
    pub router_id: String,
    pub state: Lifecycle,
    pub registrations: Vec<RegistrationStatus>,
}
impl Publication {
    pub fn status(&self) -> PublicationStatus {
        let registrations: Vec<_> = self.entries.iter().map(|entry| entry.status()).collect();
        let state = if registrations
            .iter()
            .any(|entry| entry.state == Lifecycle::Open)
        {
            Lifecycle::Open
        } else if registrations
            .iter()
            .all(|entry| entry.state == Lifecycle::Closed)
        {
            Lifecycle::Closed
        } else {
            Lifecycle::Draining
        };
        PublicationStatus {
            router_id: self.router_id.clone(),
            state,
            registrations,
        }
    }
    pub async fn abort(&self) -> Result<()> {
        self.begin_abort();
        self.close().await
    }
    pub fn begin_abort(&self) {
        for entry in &self.entries {
            entry.abort();
        }
    }
    pub fn drain(&self) {
        for entry in &self.entries {
            entry.drain();
        }
    }
    pub async fn close(&self) -> Result<()> {
        self.drain();
        let mut result = Ok(());
        for entry in &self.entries {
            if let Err(error) = entry.wait_closed().await {
                result = Err(error);
            }
        }
        result
    }
}
pub struct PublicationPlan {
    owner: Arc<Owner>,
    current: Option<Arc<Publication>>,
    prepared: Vec<PreparedRegistration>,
    bundles: Vec<MrpcBundle>,
}
impl Router {
    pub fn prepare_publication(
        &self,
        current: Option<Arc<Publication>>,
        entries: Vec<ProviderEntry>,
    ) -> Result<PublicationPlan> {
        if let Some(current) = &current
            && !current
                .owner
                .upgrade()
                .is_some_and(|owner| Arc::ptr_eq(&owner, &self.owner))
        {
            return Err(Status::new(
                "invalid_argument",
                "publication belongs to another Router",
            ));
        }
        let mut names = std::collections::BTreeSet::new();
        let mut prepared = Vec::new();
        let mut bundles = Vec::new();
        for entry in entries {
            if !entry.options.name.is_empty() && !names.insert(entry.options.name.clone()) {
                return Err(Status::new(
                    "invalid_argument",
                    "duplicate publication name",
                ));
            }
            let mut registration =
                PreparedRegistration::new(&self.owner.limits, entry.provider, entry.options, None)?;
            for bundle in entry.bundles {
                bundle.validate()?;
                registration.references.push(bundle.reference());
                bundles.push(bundle);
            }
            prepared.push(registration);
        }
        if prepared.len() > self.owner.limits.max_bindings {
            return Err(Status::exhausted("RPC publication limit exceeded"));
        }
        let plan = PublicationPlan {
            owner: self.owner.clone(),
            current,
            prepared,
            bundles,
        };
        plan.validate(&self.owner.state.lock().unwrap())?;
        self.owner.repository.check_capacity(&plan.bundles)?;
        Ok(plan)
    }
    pub fn publish(&self, entries: Vec<ProviderEntry>) -> Result<Arc<Publication>> {
        self.prepare_publication(None, entries)?.commit()
    }
}
impl PublicationPlan {
    fn validate(&self, state: &State) -> Result<()> {
        if state.closed {
            return Err(Status::new("unavailable", "RPC Router closed"));
        }
        if let Some(current) = &self.current {
            for entry in &current.entries {
                if !state
                    .registrations
                    .get(&entry.id)
                    .is_some_and(|active| Arc::ptr_eq(active, entry))
                {
                    return Err(Status::new(
                        "failed_precondition",
                        "publication changed during preparation",
                    ));
                }
            }
        }
        if state.owned.len() + self.prepared.len() > self.owner.limits.max_bindings {
            return Err(Status::exhausted("RPC publication limit exceeded"));
        }
        let replaced = self
            .current
            .as_ref()
            .map(|current| {
                current
                    .entries
                    .iter()
                    .map(|entry| entry.id)
                    .collect::<std::collections::BTreeSet<_>>()
            })
            .unwrap_or_default();
        let mut hashes = BTreeMap::new();
        for reference in state
            .registrations
            .values()
            .filter(|entry| !replaced.contains(&entry.id))
            .flat_map(|entry| &entry.references)
            .chain(self.prepared.iter().flat_map(|entry| &entry.references))
        {
            if hashes
                .insert(&reference.import_path, &reference.hash)
                .is_some_and(|hash| hash != &reference.hash)
            {
                return Err(Status::new(
                    "failed_precondition",
                    "active contract hashes conflict",
                ));
            }
        }
        Ok(())
    }
    pub fn commit(self) -> Result<Arc<Publication>> {
        let mut state = self.owner.state.lock().unwrap();
        self.validate(&state)?;
        let next = state
            .next_id
            .checked_add(self.prepared.len() as u64)
            .ok_or_else(|| Status::exhausted("RPC registration IDs exhausted"))?;
        let mut entries = Vec::new();
        self.owner.repository.publish(self.bundles)?;
        for (id, prepared) in (state.next_id..).zip(self.prepared) {
            let entry = prepared.install(&self.owner, id);
            state.registrations.insert(id, entry.clone());
            state.owned.insert(id, entry.clone());
            entries.push(entry);
        }
        if let Some(current) = self.current {
            for entry in &current.entries {
                let _admission = entry.admission.lock().unwrap();
                entry.draining.cancel();
                state.registrations.remove(&entry.id);
            }
        }
        state.next_id = next;
        state.prune_selection_counters();
        state.epoch += 1;
        self.owner.changed.send_replace(state.epoch);
        Ok(Arc::new(Publication {
            router_id: self.owner.id.clone(),
            owner: Arc::downgrade(&self.owner),
            entries,
        }))
    }
}
impl Drop for Router {
    fn drop(&mut self) {
        self.begin_shutdown();
        let registrations = {
            let mut state = self.owner.state.lock().unwrap();
            state.closed = true;
            state.owned.values().cloned().collect::<Vec<_>>()
        };
        for registration in registrations {
            registration.abort();
        }
    }
}

pub struct Registration {
    owner: Weak<Owner>,
    id: u64,
    provider: Arc<dyn Provider>,
    contract: Contract,
    options: RegistrationOptions,
    references: Vec<ContractReference>,
    healthy: AtomicBool,
    draining: Cancellation,
    aborted: Cancellation,
    admission: Mutex<()>,
    leases: Arc<Semaphore>,
    capacity: usize,
    done: watch::Receiver<Option<Result<()>>>,
}
impl Registration {
    pub fn status(&self) -> RegistrationStatus {
        RegistrationStatus {
            id: self.id,
            name: self.options.name.clone(),
            state: if self.done.borrow().is_some() {
                Lifecycle::Closed
            } else if self.draining.is_cancelled() {
                Lifecycle::Draining
            } else {
                Lifecycle::Open
            },
            healthy: self.healthy.load(Ordering::Acquire),
            active_leases: self.active_leases(),
            retired: self.draining.is_cancelled(),
        }
    }
    pub fn abort(&self) {
        self.drain();
        self.aborted.cancel();
    }
    pub fn id(&self) -> u64 {
        self.id
    }
    pub fn active_leases(&self) -> usize {
        self.capacity - self.leases.available_permits()
    }
    pub fn set_healthy(&self, healthy: bool) {
        if self.healthy.swap(healthy, Ordering::AcqRel) != healthy
            && let Some(owner) = self.owner.upgrade()
        {
            let mut state = owner.state.lock().unwrap();
            state.epoch += 1;
            owner.changed.send_replace(state.epoch);
        }
    }
    pub fn drain(&self) {
        {
            let _admission = self.admission.lock().unwrap();
            self.draining.cancel();
        }
        if let Some(owner) = self.owner.upgrade() {
            let mut state = owner.state.lock().unwrap();
            if state.registrations.remove(&self.id).is_some() {
                state.prune_selection_counters();
                state.epoch += 1;
                owner.changed.send_replace(state.epoch);
            }
        }
    }
    pub async fn close(&self) -> Result<()> {
        self.drain();
        self.wait_closed().await
    }
    async fn wait_closed(&self) -> Result<()> {
        let mut done = self.done.clone();
        let result = done
            .wait_for(|value| value.is_some())
            .await
            .map_err(|_| Status::new("internal", "RPC registration owner failed"))?;
        result.as_ref().unwrap().clone()
    }
}

impl Binder for Router {
    fn bind(
        &self,
        context: CallContext,
        mut request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<RouteSet>>> {
        Box::pin(async move {
            context.check()?;
            if request.hops == 0 {
                request.hops = 16;
            }
            if request.hops < 0 {
                return Err(Status::exhausted("RPC Router hop limit exceeded"));
            }
            request.contract = request.contract.normalized(&self.owner.limits)?;
            if let Some(authorizer) = &self.owner.authorizer {
                authorizer(
                    context.clone(),
                    request.peer.clone(),
                    request.contract.clone(),
                )
                .await?;
            }
            let mut groups = BTreeMap::<String, Vec<Method>>::new();
            for method in &request.contract.methods {
                if method.resource_type_hash.is_empty() {
                    groups
                        .entry(method.service.clone())
                        .or_default()
                        .push(method.clone());
                }
            }
            let mut providers: Vec<Arc<dyn Provider>> = Vec::new();
            {
                let mut state = self.owner.state.lock().unwrap();
                if state.closed {
                    return Err(Status::new("unavailable", "RPC Router closed"));
                }
                for (service, methods) in &groups {
                    let required = if groups.len() == 1 {
                        request.contract.clone()
                    } else {
                        Contract::new(methods.clone())
                    };
                    let mut rejected = Status::new(
                        "unimplemented",
                        format!("RPC service is not implemented: {service}"),
                    );
                    let mut candidates = Vec::new();
                    for entry in state.registrations.values() {
                        if let Err(error) = entry.contract.check_support(&required) {
                            rejected = prefer_error(rejected, error);
                            continue;
                        }
                        if entry.draining.is_cancelled()
                            || !entry.healthy.load(Ordering::Acquire)
                            || !request
                                .options
                                .labels
                                .iter()
                                .all(|(key, value)| entry.options.labels.get(key) == Some(value))
                        {
                            rejected = prefer_error(
                                rejected,
                                Status::new("unavailable", "RPC provider is not selectable"),
                            );
                            continue;
                        }
                        candidates.push(entry.clone());
                    }
                    if candidates.is_empty() {
                        return Err(rejected);
                    }
                    candidates.sort_by(|a, b| {
                        b.options.priority.cmp(&a.options.priority).then_with(|| {
                            if request.options.affinity_key.is_empty() {
                                a.id.cmp(&b.id)
                            } else {
                                affinity(&request.options.affinity_key, b.id)
                                    .cmp(&affinity(&request.options.affinity_key, a.id))
                            }
                        })
                    });
                    if request.options.affinity_key.is_empty() {
                        let mut start = 0;
                        while start < candidates.len() {
                            let mut end = start + 1;
                            while end < candidates.len()
                                && candidates[end].options.priority
                                    == candidates[start].options.priority
                            {
                                end += 1;
                            }
                            let weight = candidates[start..end]
                                .iter()
                                .try_fold(0u64, |sum, entry| sum.checked_add(entry.options.weight))
                                .ok_or_else(|| Status::exhausted("RPC routing weight overflow"))?;
                            let cursor = state.round_robin.entry(service.clone()).or_default();
                            let mut position = *cursor % weight;
                            *cursor = cursor.wrapping_add(1);
                            let mut chosen = start;
                            for (index, entry) in
                                candidates.iter().enumerate().take(end).skip(start)
                            {
                                if position < entry.options.weight {
                                    chosen = index;
                                    break;
                                }
                                position -= entry.options.weight;
                            }
                            candidates[start..=chosen].rotate_right(1);
                            start = end;
                        }
                    }
                    let mut declared = methods.clone();
                    for entry in &candidates {
                        for method in &entry.contract.methods {
                            if !method.resource_type_hash.is_empty() && !declared.contains(method) {
                                declared.push(method.clone());
                            }
                        }
                    }
                    providers.push(Arc::new(Selection {
                        runtime: self.owner.runtime.clone(),
                        candidates,
                        contract: Contract::new(declared),
                    }));
                }
            }
            let binder = LocalBinder::new(
                self.owner.runtime.clone(),
                self.owner.limits.clone(),
                providers,
            )?;
            binder.bind(context, request).await
        })
    }
}

fn affinity(key: &str, id: u64) -> u64 {
    key.bytes()
        .chain(id.to_le_bytes())
        .fold(0xcbf2_9ce4_8422_2325_u64, |hash, byte| {
            (hash ^ u64::from(byte)).wrapping_mul(0x0100_0000_01b3)
        })
}
fn prefer_error(current: Status, next: Status) -> Status {
    let rank = |code: &str| match code {
        "unimplemented" => 0,
        "failed_precondition" => 1,
        "unavailable" => 2,
        "resource_exhausted" => 3,
        _ => 4,
    };
    if rank(&next.code) > rank(&current.code)
        || rank(&next.code) == rank(&current.code)
            && (&next.code, &next.message) < (&current.code, &current.message)
    {
        next
    } else {
        current
    }
}

struct Selection {
    runtime: Handle,
    candidates: Vec<Arc<Registration>>,
    contract: Contract,
}
impl Provider for Selection {
    fn contract(&self) -> Contract {
        self.contract.clone()
    }
    fn bind(
        &self,
        context: CallContext,
        mut request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<dyn ProviderLease>>> {
        Box::pin(async move {
            request.hops -= 1;
            let mut error = Status::new("unavailable", "RPC provider unavailable");
            for entry in &self.candidates {
                let permit = {
                    let _admission = entry.admission.lock().unwrap();
                    if entry.draining.is_cancelled() || !entry.healthy.load(Ordering::Acquire) {
                        continue;
                    }
                    match entry.leases.clone().try_acquire_owned() {
                        Ok(permit) => permit,
                        Err(_) => {
                            error = prefer_error(
                                error,
                                Status::exhausted("RPC provider lease limit exceeded"),
                            );
                            continue;
                        }
                    }
                };
                request.provider = entry.options.name.clone();
                let result = tokio::select! {
                    _ = entry.draining.cancelled() => Err(Status::new("unavailable", "RPC provider draining")),
                    result = context.run(entry.provider.bind(context.clone(),request.clone())) => result,
                };
                match result {
                    Ok(lease) => {
                        if entry.draining.is_cancelled() {
                            let _ = lease.close().await;
                            continue;
                        }
                        let closing = Cancellation::default();
                        let (done, receiver) = watch::channel(None);
                        let cleanup = lease.clone();
                        let closed = closing.clone();
                        let aborted = entry.aborted.clone();
                        self.runtime.spawn(async move {
                            tokio::select! { _ = closed.cancelled() => {}, _ = aborted.cancelled() => {} }
                            closed.cancel();
                            let result = cleanup.close().await;
                            drop(permit);
                            done.send_replace(Some(result));
                        });
                        return Ok(Arc::new(RegisteredLease {
                            lease,
                            closing,
                            done: receiver,
                        }) as Arc<dyn ProviderLease>);
                    }
                    Err(next) => error = prefer_error(error, next),
                }
            }
            Err(error)
        })
    }
}
struct RegisteredLease {
    lease: Arc<dyn ProviderLease>,
    closing: Cancellation,
    done: watch::Receiver<Option<Result<()>>>,
}
impl ProviderLease for RegisteredLease {
    fn invoke(
        &self,
        context: CallContext,
        method: Method,
        arguments: Vec<Value>,
    ) -> BoxFuture<'_, Result<ProviderResult>> {
        Box::pin(async move {
            tokio::select! {
                biased;
                _ = self.closing.cancelled() => Err(Status::new("unavailable", "RPC registration closed")),
                result = self.lease.invoke(context, method, arguments) => result,
            }
        })
    }
    fn close(&self) -> BoxFuture<'_, Result<()>> {
        self.closing.cancel();
        Box::pin(async move {
            let mut done = self.done.clone();
            let result = done
                .wait_for(|value| value.is_some())
                .await
                .map_err(|_| Status::new("internal", "RPC provider cleanup failed"))?;
            result.as_ref().unwrap().clone()
        })
    }
}
impl Drop for RegisteredLease {
    fn drop(&mut self) {
        self.closing.cancel();
    }
}

impl ProviderPublisher for Router {
    fn publish(
        &self,
        context: CallContext,
        name: String,
        provider: Arc<dyn Provider>,
    ) -> BoxFuture<'_, Result<Arc<dyn ProviderPublication>>> {
        Box::pin(async move {
            context.check()?;
            Ok(self.register(
                provider,
                RegistrationOptions {
                    name,
                    ..RegistrationOptions::default()
                },
            )? as Arc<dyn ProviderPublication>)
        })
    }
}
impl ProviderPublication for Registration {
    fn close(&self) -> BoxFuture<'_, Result<()>> {
        self.abort();
        Box::pin(Registration::close(self))
    }
}
