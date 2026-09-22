use crate::language::LanguageService;
use crate::{error::RuntimeError, ffi::Cancellation};
use serde_json::{Value, json};
use std::collections::{BTreeMap, BTreeSet};

pub struct LanguageServer {
    service: LanguageService,
    workspace: Value,
    documents: BTreeMap<String, (String, String)>,
    published: BTreeMap<String, String>,
    initialized: bool,
    shutdown: bool,
}
impl LanguageServer {
    pub async fn new(image: &[u8], workspace: Value) -> Result<Self, RuntimeError> {
        Ok(Self {
            service: LanguageService::new(image).await?,
            workspace,
            documents: BTreeMap::new(),
            published: BTreeMap::new(),
            initialized: false,
            shutdown: false,
        })
    }
    pub async fn dispatch(&mut self, message: Value, cancel: &Cancellation) -> Vec<Value> {
        let id = message.get("id").cloned();
        let method = message["method"].as_str().unwrap_or("");
        let params = &message["params"];
        let result = self.execute(method, params, cancel).await;
        let mut messages = Vec::new();
        match result {
            Ok((value, notifications)) => {
                if let Some(id) = id {
                    messages.push(json!({"jsonrpc":"2.0","id":id,"result":value}));
                }
                messages.extend(notifications);
            }
            Err(error) => {
                if let Some(id) = id {
                    let code = match error.code {
                        "canceled" => -32800,
                        "stale" => -32801,
                        "not_initialized" => -32002,
                        "method_not_found" => -32601,
                        _ => -32602,
                    };
                    messages.push(json!({"jsonrpc":"2.0","id":id,"error":{"code":code,"message":error.to_string()}}));
                } else {
                    messages.push(json!({"jsonrpc":"2.0","method":"window/logMessage","params":{"type":1,"message":error.to_string()}}));
                }
            }
        }
        messages
    }
    pub async fn close(&mut self) -> Result<(), RuntimeError> {
        self.shutdown = true;
        self.service.close().await
    }
    async fn execute(
        &mut self,
        method: &str,
        params: &Value,
        cancel: &Cancellation,
    ) -> Result<(Value, Vec<Value>), RuntimeError> {
        if method == "initialize" {
            if self.initialized {
                return Err(failure("already initialized"));
            }
            if params["initializationOptions"]["workspace"].is_object() {
                self.workspace = params["initializationOptions"]["workspace"].clone();
            }
            self.service.open(self.workspace.clone(), cancel).await?;
            self.service.analyze(cancel).await?;
            self.documents = document_map(&self.workspace);
            self.initialized = true;
            return Ok((
                json!({
                    "capabilities": {
                        "positionEncoding": "utf-16",
                        "textDocumentSync": {"openClose": true, "change": 2, "save": true},
                        "hoverProvider": true,
                        "completionProvider": {"resolveProvider": true, "triggerCharacters": ["."]},
                        "signatureHelpProvider": {"triggerCharacters": ["(", ","]},
                        "definitionProvider": true,
                        "typeDefinitionProvider": true,
                        "implementationProvider": true,
                        "referencesProvider": true,
                        "documentHighlightProvider": true,
                        "documentSymbolProvider": true,
                        "workspaceSymbolProvider": true,
                        "renameProvider": {"prepareProvider": true},
                        "documentFormattingProvider": true,
                        "documentRangeFormattingProvider": true,
                        "documentOnTypeFormattingProvider": {"firstTriggerCharacter": "}", "moreTriggerCharacter": [";", "\n"]},
                        "foldingRangeProvider": true,
                        "selectionRangeProvider": true,
                        "codeActionProvider": {"resolveProvider": true},
                        "diagnosticProvider": {"interFileDependencies": true, "workspaceDiagnostics": false},
                        "semanticTokensProvider": {
                            "legend": {
                                "tokenTypes": ["namespace", "type", "typeParameter", "parameter", "variable", "property", "function", "method", "keyword", "comment", "string", "number", "operator"],
                                "tokenModifiers": ["declaration", "readonly"],
                            },
                            "full": true, "range": true,
                        },
                    },
                    "serverInfo": {"name": "mini-go-rust", "version": env!("CARGO_PKG_VERSION")},
                }),
                vec![],
            ));
        }
        if !self.initialized {
            return Err(RuntimeError::new(
                "not_initialized",
                "lsp",
                "server not initialized",
            ));
        }
        if method == "exit" {
            self.close().await?;
            return Ok((Value::Null, vec![]));
        }
        if self.shutdown {
            return Err(failure("server shut down"));
        }
        if method == "shutdown" {
            self.close().await?;
            return Ok((Value::Null, vec![]));
        }
        if method == "initialized" {
            return Ok((Value::Null, self.diagnostics(cancel).await?));
        }
        let uri = params["textDocument"]["uri"].as_str().unwrap_or("");
        match method {
            "textDocument/didOpen" | "textDocument/didChange" | "textDocument/didClose" => {
                let mut pending_document = None;
                if method == "textDocument/didOpen" && !self.documents.contains_key(uri) {
                    let (directory, name) = uri
                        .rsplit_once('/')
                        .ok_or_else(|| failure("invalid document URI"))?;
                    let identity = self
                        .documents
                        .iter()
                        .find_map(|(known, (module, path))| {
                            if known.rsplit_once('/').map(|(parent, _)| parent) != Some(directory) {
                                return None;
                            }
                            let path = path
                                .rsplit_once('/')
                                .map(|(parent, _)| format!("{parent}/{name}"))
                                .unwrap_or_else(|| name.to_owned());
                            Some((module.clone(), path))
                        })
                        .ok_or_else(|| failure("document is outside prepared workspace"))?;
                    pending_document = Some(identity);
                }
                let (module, path) = self
                    .documents
                    .get(uri)
                    .or(pending_document.as_ref())
                    .ok_or_else(|| failure("document is outside prepared workspace"))?;
                let operation = match method {
                    "textDocument/didOpen" => "open",
                    "textDocument/didChange" => "change",
                    _ => "close",
                };
                self.service.update(json!([{"Operation":operation,"Identity":{"URI":uri,"ModulePath":module,"Path":path},"Version":params["textDocument"]["version"].as_i64().unwrap_or(0),"Text":params["textDocument"]["text"].as_str().unwrap_or(""),"Changes":params["contentChanges"].as_array().cloned().unwrap_or_default()}]),cancel).await?;
                if let Some(identity) = pending_document {
                    self.documents.insert(uri.to_owned(), identity);
                }
                if operation == "close" && !document_map(&self.workspace).contains_key(uri) {
                    self.documents.remove(uri);
                }
                return Ok((Value::Null, self.diagnostics(cancel).await?));
            }
            "textDocument/didSave" => {
                let confirmed = self
                    .service
                    .session
                    .confirmed_input()
                    .ok_or_else(|| failure("workspace is not open"))?;
                let change = confirmed["Changes"]
                    .as_array()
                    .into_iter()
                    .flatten()
                    .find(|change| change["Identity"]["URI"] == uri);
                if let Some(change) = change {
                    let mut workspace = self.workspace.clone();
                    for package in workspace["Packages"].as_array_mut().into_iter().flatten() {
                        if package["ModulePath"] != change["Identity"]["ModulePath"] {
                            continue;
                        }
                        let mut found = false;
                        for key in ["Files", "TestFiles"] {
                            for file in package[key].as_array_mut().into_iter().flatten() {
                                if file["Path"] == change["Identity"]["Path"] {
                                    file["Text"] = change["Text"].clone();
                                    found = true;
                                }
                            }
                        }
                        if !found {
                            let key = if change["Identity"]["Path"]
                                .as_str()
                                .unwrap_or("")
                                .ends_with("_test.mgo")
                            {
                                "TestFiles"
                            } else {
                                "Files"
                            };
                            if !package[key].is_array() {
                                package[key] = json!([]);
                            }
                            package[key].as_array_mut().unwrap().push(json!({"Path":change["Identity"]["Path"],"URI":uri,"Text":change["Text"]}));
                        }
                    }
                    return Ok((
                        Value::Null,
                        self.replace_workspace(workspace, cancel).await?,
                    ));
                }
                return Ok((Value::Null, vec![]));
            }
            _ => {}
        }
        let operation = match method {
            "textDocument/hover" => "hover",
            "textDocument/completion" => "completion",
            "completionItem/resolve" => "completion/resolve",
            "textDocument/signatureHelp" => "signatureHelp",
            "textDocument/definition" => "definition",
            "textDocument/typeDefinition" => "typeDefinition",
            "textDocument/implementation" => "implementation",
            "textDocument/references" => "references",
            "textDocument/documentHighlight" => "documentHighlight",
            "textDocument/documentSymbol" => "documentSymbol",
            "workspace/symbol" => "workspaceSymbol",
            "textDocument/prepareRename" => "prepareRename",
            "textDocument/rename" => "rename",
            "textDocument/diagnostic" => "diagnostic",
            "textDocument/formatting" => "formatting",
            "textDocument/rangeFormatting" => "rangeFormatting",
            "textDocument/onTypeFormatting" => "onTypeFormatting",
            "textDocument/semanticTokens/full" | "textDocument/semanticTokens/range" => {
                "semanticTokens"
            }
            "textDocument/foldingRange" => "foldingRange",
            "textDocument/selectionRange" => "selectionRange",
            "textDocument/codeAction" => "codeAction",
            "codeAction/resolve" => "codeAction/resolve",
            _ => {
                return Err(RuntimeError::new(
                    "method_not_found",
                    "lsp",
                    "unknown LSP request",
                ));
            }
        };
        let mut query = json!({"URI":uri,"Position":params["position"],"Range":params["range"],"Positions":params["positions"],"IncludeDeclaration":params["context"]["includeDeclaration"].as_bool().unwrap_or(false)});
        for field in ["Position", "Range", "Positions"] {
            if query[field].is_null() {
                query.as_object_mut().unwrap().remove(field);
            }
        }
        query["Text"] = params["newName"]
            .as_str()
            .or(params["query"].as_str())
            .or(params["ch"].as_str())
            .or(params["previousResultId"].as_str())
            .unwrap_or("")
            .into();
        if operation == "completion/resolve" {
            query["Completion"] = params.clone();
        }
        if operation == "codeAction/resolve" {
            query["Action"] = params.clone();
        }
        if operation == "codeAction" {
            query["Diagnostics"] = params["context"]["diagnostics"].clone();
        }
        let result = self.service.query(operation, query, cancel).await?;
        Ok((result, vec![]))
    }
    async fn diagnostics(&mut self, cancel: &Cancellation) -> Result<Vec<Value>, RuntimeError> {
        let analyzed = self.service.analyze(cancel).await?;
        let reports = analyzed["Analysis"]["Diagnostics"]
            .as_object()
            .ok_or_else(|| failure("missing diagnostics"))?;
        let mut messages = Vec::new();
        let current: BTreeSet<String> = reports
            .keys()
            .filter(|uri| self.documents.contains_key(*uri))
            .cloned()
            .collect();
        for uri in self.published.keys().filter(|uri| !current.contains(*uri)) {
            messages.push(json!({"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":uri,"diagnostics":[]}}));
        }
        for uri in &current {
            if self.published.get(uri).map(String::as_str) == reports[uri]["resultId"].as_str() {
                continue;
            }
            messages.push(json!({"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":uri,"diagnostics":reports[uri]["items"].as_array().cloned().unwrap_or_default()}}));
        }
        self.published = current
            .into_iter()
            .map(|uri| {
                let id = reports[&uri]["resultId"].as_str().unwrap_or("").to_owned();
                (uri, id)
            })
            .collect();
        Ok(messages)
    }
    /// A host watcher prepares sources asynchronously before publishing them here.
    pub async fn replace_workspace(
        &mut self,
        mut workspace: Value,
        cancel: &Cancellation,
    ) -> Result<Vec<Value>, RuntimeError> {
        workspace["Operation"] = "workspace/update".into();
        self.service.session.call(workspace.clone(), cancel).await?;
        workspace.as_object_mut().unwrap().remove("Operation");
        let mut documents = document_map(&workspace);
        if let Some(recovery) = self.service.session.confirmed_input() {
            for change in recovery["Changes"].as_array().into_iter().flatten() {
                let identity = &change["Identity"];
                if let (Some(uri), Some(module), Some(path)) = (
                    identity["URI"].as_str(),
                    identity["ModulePath"].as_str(),
                    identity["Path"].as_str(),
                ) {
                    documents.insert(uri.to_owned(), (module.to_owned(), path.to_owned()));
                }
            }
        }
        self.workspace = workspace;
        self.documents = documents;
        self.diagnostics(cancel).await
    }
}
fn document_map(workspace: &Value) -> BTreeMap<String, (String, String)> {
    let mut documents = BTreeMap::new();
    for package in workspace["Packages"].as_array().into_iter().flatten() {
        let module = package["ModulePath"].as_str().unwrap_or("");
        for file in package["Files"]
            .as_array()
            .into_iter()
            .flatten()
            .chain(package["TestFiles"].as_array().into_iter().flatten())
        {
            let path = file["Path"].as_str().unwrap_or("");
            let uri = file["URI"]
                .as_str()
                .map(str::to_owned)
                .unwrap_or_else(|| format!("mini-go://{module}/{path}"));
            documents.insert(uri, (module.into(), path.into()));
        }
    }
    documents
}
fn failure(message: &str) -> RuntimeError {
    RuntimeError::new("invalid_argument", "lsp", message)
}
