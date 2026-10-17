use super::catalog::{ContractReference, ContractRepository, MrpcBundle, MrpcFile};
use super::control as wire;
use super::platform::Handle;
use super::*;
use serde::Serialize;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::{Semaphore, oneshot, watch};

pub const PUBLICATION_PROTOCOL: &str = "minigo.rpc.gateway.publication.v3";
pub use super::catalog::SNAPSHOT_PROTOCOL;

impl From<&Method> for wire::Method {
    fn from(value: &Method) -> Self {
        Self {
            id: value.id.clone(),
            service: value.service.clone(),
            name: value.name.clone(),
            contract_hash: value.contract_hash.clone(),
            resource_type_hash: value.resource_type_hash.clone(),
        }
    }
}
impl From<&wire::Method> for Method {
    fn from(value: &wire::Method) -> Self {
        Self {
            id: value.id.clone(),
            service: value.service.clone(),
            name: value.name.clone(),
            contract_hash: value.contract_hash.clone(),
            resource_type_hash: value.resource_type_hash.clone(),
        }
    }
}
impl From<&Contract> for wire::Contract {
    fn from(value: &Contract) -> Self {
        Self {
            protocol: value.protocol.clone(),
            methods: Some(value.methods.iter().map(Into::into).collect()),
        }
    }
}
impl From<&wire::Contract> for Contract {
    fn from(value: &wire::Contract) -> Self {
        Self {
            protocol: value.protocol.clone(),
            methods: value
                .methods
                .as_deref()
                .unwrap_or_default()
                .iter()
                .map(Into::into)
                .collect(),
        }
    }
}
impl From<&ContractReference> for wire::ContractReference {
    fn from(value: &ContractReference) -> Self {
        Self {
            import_path: value.import_path.clone(),
            hash: value.hash.clone(),
        }
    }
}
impl From<&wire::ContractReference> for ContractReference {
    fn from(value: &wire::ContractReference) -> Self {
        Self {
            import_path: value.import_path.clone(),
            hash: value.hash.clone(),
        }
    }
}
impl From<MrpcBundle> for wire::ContractBundle {
    fn from(value: MrpcBundle) -> Self {
        Self {
            import_path: value.import_path,
            hash: value.hash,
            files: Some(
                value
                    .files
                    .into_iter()
                    .map(|file| wire::ContractFile {
                        path: file.path,
                        text: file.text,
                        hash: file.hash,
                    })
                    .collect(),
            ),
        }
    }
}
impl From<wire::ContractBundle> for MrpcBundle {
    fn from(value: wire::ContractBundle) -> Self {
        Self {
            import_path: value.import_path,
            hash: value.hash,
            files: value
                .files
                .unwrap_or_default()
                .into_iter()
                .map(|file| MrpcFile {
                    path: file.path,
                    text: file.text,
                    hash: file.hash,
                })
                .collect(),
        }
    }
}

/// Validates and hashes the language-neutral Go publication JSON material.
pub fn normalize_snapshot(
    mut snapshot: wire::PublicationSnapshot,
) -> Result<wire::PublicationSnapshot> {
    if snapshot.protocol != PUBLICATION_PROTOCOL
        || snapshot.process_id.trim().is_empty()
        || snapshot.generation == 0
    {
        return Err(Status::protocol("invalid Gateway publication identity"));
    }
    snapshot.process_id = snapshot.process_id.trim().into();
    let providers = snapshot
        .providers
        .as_mut()
        .ok_or_else(|| Status::protocol("publication providers required"))?;
    if providers.is_empty() || providers.len() > 1024 {
        return Err(Status::protocol("invalid publication provider count"));
    }
    let mut names = BTreeSet::new();
    #[derive(Serialize)]
    struct MethodJson<'a> {
        #[serde(rename = "ID")]
        id: &'a str,
        #[serde(rename = "Service")]
        service: &'a str,
        #[serde(rename = "Name")]
        name: &'a str,
        #[serde(rename = "ContractHash")]
        contract_hash: &'a str,
        #[serde(rename = "ResourceTypeHash")]
        resource_type_hash: &'a str,
    }
    #[derive(Serialize)]
    struct ContractJson<'a> {
        #[serde(rename = "Protocol")]
        protocol: &'a str,
        #[serde(rename = "Methods")]
        methods: Vec<MethodJson<'a>>,
    }
    #[derive(Serialize)]
    #[serde(rename_all = "PascalCase")]
    struct OptionsJson<'a> {
        name: &'a str,
        priority: i64,
        weight: i64,
        max_leases: i64,
        labels: Option<BTreeMap<&'a str, &'a str>>,
    }
    #[derive(Serialize)]
    struct ProviderJson<'a> {
        id: &'a str,
        contract: ContractJson<'a>,
        options: OptionsJson<'a>,
        #[serde(skip_serializing_if = "Vec::is_empty")]
        references: Vec<ContractReference>,
    }
    #[derive(Serialize)]
    struct SnapshotJson<'a> {
        protocol: &'a str,
        #[serde(rename = "processID")]
        process_id: &'a str,
        generation: u64,
        id: &'a str,
        providers: Vec<ProviderJson<'a>>,
    }
    for provider in providers.iter_mut() {
        provider.id = provider.id.trim().into();
        if provider.id.is_empty() || !names.insert(provider.id.clone()) {
            return Err(Status::protocol(
                "duplicate or empty publication provider ID",
            ));
        }
        provider.options.name = provider.id.clone();
        provider.contract =
            (&Contract::from(&provider.contract).normalized(&Limits::default())?).into();
        if provider
            .options
            .labels
            .as_ref()
            .is_some_and(|v| v.len() > 256)
            || provider.references.as_ref().is_some_and(|v| v.len() > 4096)
        {
            return Err(Status::protocol(
                "publication labels or references exceed limits",
            ));
        }
        let mut references = BTreeSet::new();
        if let Some(items) = &mut provider.references {
            items.sort_by(|a, b| (&a.import_path, &a.hash).cmp(&(&b.import_path, &b.hash)));
            for reference in items {
                if reference.import_path.trim().is_empty()
                    || reference.hash.len() != 64
                    || !reference
                        .hash
                        .bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                    || !references.insert((reference.import_path.clone(), reference.hash.clone()))
                {
                    return Err(Status::protocol("invalid publication contract reference"));
                }
            }
        }
    }
    providers.sort_by(|a, b| a.id.cmp(&b.id));
    let material = SnapshotJson {
        protocol: &snapshot.protocol,
        process_id: &snapshot.process_id,
        generation: snapshot.generation,
        id: "",
        providers: providers
            .iter()
            .map(|provider| ProviderJson {
                id: &provider.id,
                contract: ContractJson {
                    protocol: &provider.contract.protocol,
                    methods: provider
                        .contract
                        .methods
                        .as_deref()
                        .unwrap_or_default()
                        .iter()
                        .map(|method| MethodJson {
                            id: &method.id,
                            service: &method.service,
                            name: &method.name,
                            contract_hash: &method.contract_hash,
                            resource_type_hash: &method.resource_type_hash,
                        })
                        .collect(),
                },
                options: OptionsJson {
                    name: &provider.options.name,
                    priority: provider.options.priority,
                    weight: provider.options.weight,
                    max_leases: provider.options.max_leases,
                    labels: provider
                        .options
                        .labels
                        .as_ref()
                        .filter(|v| !v.is_empty())
                        .map(|labels| {
                            labels
                                .iter()
                                .map(|label| (label.name.as_str(), label.value.as_str()))
                                .collect()
                        }),
                },
                references: provider
                    .references
                    .as_deref()
                    .unwrap_or_default()
                    .iter()
                    .map(Into::into)
                    .collect(),
            })
            .collect(),
    };
    let id = format!("{:x}", Sha256::digest(super::catalog::go_json(&material)?));
    if !snapshot.id.is_empty() && snapshot.id != id {
        return Err(Status::protocol("publication snapshot hash mismatch"));
    }
    snapshot.id = id;
    Ok(snapshot)
}

pub struct PublicationProvider {
    pub id: String,
    pub provider: Arc<dyn Provider>,
    pub bundles: Vec<MrpcBundle>,
}
pub struct Publisher {
    snapshot: wire::PublicationSnapshot,
    repository: ContractRepository,
    state: watch::Sender<u8>,
}
impl Publisher {
    pub fn new(
        process_id: String,
        generation: u64,
        providers: Vec<PublicationProvider>,
    ) -> Result<Arc<Self>> {
        let repository = ContractRepository::default();
        let mut declarations = Vec::new();
        for entry in providers {
            let id = if entry.id.is_empty() {
                entry.provider.provider_id()
            } else {
                entry.id
            };
            let references = entry
                .bundles
                .iter()
                .map(|bundle| wire::ContractReference::from(&bundle.reference()))
                .collect::<Vec<_>>();
            repository.publish(entry.bundles)?;
            declarations.push(wire::PublishedProvider {
                id: id.clone(),
                contract: (&entry.provider.contract()).into(),
                options: wire::RegistrationOptions {
                    name: id,
                    priority: 0,
                    weight: 0,
                    max_leases: 0,
                    labels: None,
                },
                references: (!references.is_empty()).then_some(references),
            });
        }
        let snapshot = normalize_snapshot(wire::PublicationSnapshot {
            protocol: PUBLICATION_PROTOCOL.into(),
            process_id,
            generation,
            id: String::new(),
            providers: Some(declarations),
        })?;
        let (state, _) = watch::channel(0);
        Ok(Arc::new(Self {
            snapshot,
            repository,
            state,
        }))
    }
    pub fn provider(self: &Arc<Self>) -> Result<Arc<dyn Provider>> {
        wire::publication_provider(self.clone())
    }
    pub async fn wait_published(&self, context: CallContext) -> Result<()> {
        context
            .run(async {
                let mut state = self.state.subscribe();
                state
                    .wait_for(|value| *value == 2)
                    .await
                    .map_err(|_| Status::new("unavailable", "publication closed"))?;
                Ok(())
            })
            .await
    }
}
impl wire::PublicationHandler for Publisher {
    fn snapshot(&self, _: CallContext) -> BoxFuture<'_, Result<(wire::PublicationSnapshot,)>> {
        Box::pin(async { Ok((self.snapshot.clone(),)) })
    }
    fn resolve(
        &self,
        _: CallContext,
        import_path: String,
        hash: String,
    ) -> BoxFuture<'_, Result<(wire::ContractBundle,)>> {
        Box::pin(async move {
            Ok((self
                .repository
                .resolve(&ContractReference { import_path, hash })?
                .into(),))
        })
    }
    fn ready(&self, _: CallContext, id: String) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            if id != self.snapshot.id {
                return Err(Status::protocol("publication Ready identity mismatch"));
            }
            self.state.send_if_modified(|state| {
                if *state == 0 {
                    *state = 1;
                    true
                } else {
                    false
                }
            });
            Ok(())
        })
    }
    fn published(&self, _: CallContext, id: String) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            if id != self.snapshot.id || *self.state.borrow() == 0 {
                return Err(Status::protocol("publication is not ready"));
            }
            self.state.send_replace(2);
            Ok(())
        })
    }
}

#[derive(Clone)]
pub struct PublicationRegistryOptions {
    pub control_timeout: Duration,
    pub drain_timeout: Duration,
    pub max_publications: usize,
}
impl Default for PublicationRegistryOptions {
    fn default() -> Self {
        Self {
            control_timeout: Duration::from_secs(10),
            drain_timeout: Duration::from_secs(30),
            max_publications: 1024,
        }
    }
}
struct RemotePublication {
    generation: u64,
    id: String,
    token: String,
    peer: String,
    endpoint: Arc<Endpoint>,
    publication: Arc<router::Publication>,
    _permit: tokio::sync::OwnedSemaphorePermit,
}
impl Drop for RemotePublication {
    fn drop(&mut self) {
        self.publication.begin_abort();
        self.endpoint.begin_shutdown();
    }
}
#[derive(Default)]
struct RegistryState {
    active: BTreeMap<String, Arc<RemotePublication>>,
    preparing: BTreeMap<String, String>,
}
pub struct PublicationRegistry {
    runtime: Handle,
    slots: Arc<Semaphore>,
    router: Arc<router::Router>,
    options: PublicationRegistryOptions,
    next_token: std::sync::atomic::AtomicU64,
    state: std::sync::Mutex<RegistryState>,
}
impl Drop for PublicationRegistry {
    fn drop(&mut self) {
        let active = std::mem::take(&mut self.state.get_mut().unwrap().active);
        for (_, entry) in active {
            entry.publication.begin_abort();
            entry.endpoint.begin_shutdown();
            self.runtime.spawn(async move {
                let _ = entry.publication.abort().await;
                let _ = entry.endpoint.shutdown().await;
            });
        }
    }
}
struct Preparation {
    registry: Arc<PublicationRegistry>,
    process: String,
    token: String,
}
struct Attachment {
    registry: Arc<PublicationRegistry>,
    identity: Option<(String, String)>,
}
impl Drop for Attachment {
    fn drop(&mut self) {
        if let Some((process, token)) = self.identity.take() {
            let registry = self.registry.clone();
            self.registry.runtime.spawn(async move {
                let _ = registry.detach(&process, &token).await;
            });
        }
    }
}
impl Drop for Preparation {
    fn drop(&mut self) {
        let mut state = self.registry.state.lock().unwrap();
        if state.preparing.get(&self.process) == Some(&self.token) {
            state.preparing.remove(&self.process);
        }
    }
}
impl PublicationRegistry {
    pub fn new(runtime: Handle, router: Arc<router::Router>) -> Arc<Self> {
        Self::with_options(runtime, router, PublicationRegistryOptions::default())
            .expect("default publication options")
    }
    pub fn with_options(
        runtime: Handle,
        router: Arc<router::Router>,
        options: PublicationRegistryOptions,
    ) -> Result<Arc<Self>> {
        if options.max_publications == 0
            || options.max_publications > u32::MAX as usize
            || options.control_timeout.is_zero()
            || options.drain_timeout.is_zero()
        {
            return Err(Status::new(
                "invalid_argument",
                "invalid publication limits",
            ));
        }
        Ok(Arc::new(Self {
            runtime,
            slots: Arc::new(Semaphore::new(options.max_publications)),
            router,
            options,
            next_token: std::sync::atomic::AtomicU64::new(1),
            state: std::sync::Mutex::new(RegistryState::default()),
        }))
    }
    pub async fn attach(
        self: &Arc<Self>,
        endpoint: Arc<Endpoint>,
        peer: PeerInfo,
    ) -> Result<(String, String)> {
        let permit = self
            .slots
            .clone()
            .try_acquire_owned()
            .map_err(|_| Status::exhausted("Gateway publication limit exceeded"))?;
        let (sender, receiver) = oneshot::channel();
        let registry = self.clone();
        self.runtime.spawn(async move {
            let result = registry.attach_owned(endpoint, peer, permit, &sender).await;
            let _ = sender.send(result.map(|identity| Attachment {
                registry,
                identity: Some(identity),
            }));
        });
        let mut receipt = receiver
            .await
            .map_err(|_| Status::new("internal", "Gateway publication owner failed"))??;
        Ok(receipt.identity.take().unwrap())
    }
    async fn attach_owned(
        self: &Arc<Self>,
        endpoint: Arc<Endpoint>,
        peer: PeerInfo,
        permit: tokio::sync::OwnedSemaphorePermit,
        delivery: &oneshot::Sender<Result<Attachment>>,
    ) -> Result<(String, String)> {
        let context =
            CallContext::with_deadline(std::time::Instant::now() + self.options.control_timeout);
        let client = wire::PublicationClient::bind(
            context.clone(),
            endpoint.as_ref(),
            BindOptions::default(),
        )
        .await?;
        let snapshot = normalize_snapshot(client.snapshot(context.clone()).await?.0)?;
        let sequence = self
            .next_token
            .try_update(
                std::sync::atomic::Ordering::AcqRel,
                std::sync::atomic::Ordering::Acquire,
                |id| id.checked_add(1),
            )
            .map_err(|_| Status::exhausted("publication token space exhausted"))?;
        let token = format!("{}-{sequence}", snapshot.id);
        let current = {
            let mut state = self.state.lock().unwrap();
            let current = state.active.get(&snapshot.process_id).cloned();
            if let Some(current) = &current {
                if current.peer != peer.identity {
                    return Err(Status::new(
                        "permission_denied",
                        "publication peer identity changed",
                    ));
                }
                if snapshot.generation < current.generation
                    || snapshot.generation == current.generation && snapshot.id != current.id
                {
                    return Err(Status::new(
                        "failed_precondition",
                        "publication identity or generation is stale",
                    ));
                }
            }
            state
                .preparing
                .insert(snapshot.process_id.clone(), token.clone());
            current
        };
        let _preparation = Preparation {
            registry: self.clone(),
            process: snapshot.process_id.clone(),
            token: token.clone(),
        };
        let mut entries = Vec::new();
        let mut resolved = BTreeMap::new();
        for provider in snapshot.providers.as_deref().unwrap_or_default() {
            let mut bundles = Vec::new();
            for reference in provider.references.as_deref().unwrap_or_default() {
                let reference = ContractReference::from(reference);
                if let std::collections::btree_map::Entry::Vacant(entry) =
                    resolved.entry(reference.clone())
                {
                    let bundle = MrpcBundle::from(
                        client
                            .resolve(
                                context.clone(),
                                reference.import_path.clone(),
                                reference.hash.clone(),
                            )
                            .await?
                            .0,
                    );
                    bundle.validate()?;
                    if bundle.reference() != reference {
                        return Err(Status::protocol("publication resolved another bundle"));
                    }
                    entry.insert(bundle);
                }
                bundles.push(resolved[&reference].clone());
            }
            entries.push(router::ProviderEntry {
                provider: Arc::new(router::MountedProvider::new(
                    endpoint.clone(),
                    Contract::from(&provider.contract),
                    &Limits::default(),
                )?),
                options: router::RegistrationOptions {
                    name: provider.id.clone(),
                    priority: provider.options.priority,
                    weight: provider.options.weight.max(1) as u64,
                    max_leases: provider.options.max_leases.max(0) as usize,
                    labels: provider
                        .options
                        .labels
                        .as_deref()
                        .unwrap_or_default()
                        .iter()
                        .map(|label| (label.name.clone(), label.value.clone()))
                        .collect(),
                },
                bundles,
            });
        }
        let plan = self.router.prepare_publication(
            current.as_ref().map(|current| current.publication.clone()),
            entries,
        )?;
        client.ready(context.clone(), snapshot.id.clone()).await?;
        if delivery.is_closed() {
            return Err(Status::new("canceled", "publication caller dropped"));
        }
        {
            let mut state = self.state.lock().unwrap();
            if state.preparing.get(&snapshot.process_id) != Some(&token)
                || state
                    .active
                    .get(&snapshot.process_id)
                    .map(|value| &value.token)
                    != current.as_ref().map(|value| &value.token)
            {
                return Err(Status::new(
                    "failed_precondition",
                    "publication superseded during preparation",
                ));
            }
            let publication = plan.commit()?;
            state.active.insert(
                snapshot.process_id.clone(),
                Arc::new(RemotePublication {
                    generation: snapshot.generation,
                    id: snapshot.id.clone(),
                    token: token.clone(),
                    peer: peer.identity,
                    endpoint: endpoint.clone(),
                    publication,
                    _permit: permit,
                }),
            );
        }
        if let Some(current) = current {
            let timeout = self.options.drain_timeout;
            self.runtime.spawn(async move {
                if !matches!(
                    super::platform::timeout(timeout, current.publication.close()).await,
                    Ok(Ok(()))
                ) {
                    let _ = current.publication.abort().await;
                }
                let _ = current.endpoint.shutdown().await;
            });
        }
        let acknowledged = async {
            let mut delay = Duration::from_millis(25);
            loop {
                match client.published(context.clone(), snapshot.id.clone()).await {
                    Ok(()) => return Ok(()),
                    Err(error) => {
                        if context.check().is_err() {
                            return Err(error);
                        }
                        context
                            .run(async {
                                super::platform::sleep(delay).await;
                                Ok(())
                            })
                            .await?;
                        delay = (delay * 2).min(Duration::from_secs(1));
                    }
                }
            }
        }
        .await;
        let closed = client.close().await;
        if let Err(error) = acknowledged.and(closed) {
            self.detach(&snapshot.process_id, &token).await?;
            return Err(error);
        }
        Ok((snapshot.process_id, token))
    }
    pub async fn detach(&self, process: &str, token: &str) -> Result<()> {
        let removed = {
            let mut state = self.state.lock().unwrap();
            if state
                .active
                .get(process)
                .is_some_and(|entry| entry.token == token)
            {
                state.active.remove(process)
            } else {
                None
            }
        };
        if let Some(entry) = removed {
            entry.publication.abort().await?;
        }
        Ok(())
    }
    pub fn catalog_provider(&self) -> Result<Arc<dyn Provider>> {
        super::catalog::provider(self.router.clone())
    }
}
