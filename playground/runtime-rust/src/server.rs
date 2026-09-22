//! Native LSP/DAP connection ownership and request scheduling.
use crate::ffi::Cancellation;
use crate::{compiler::CompilerSession, dap::DebugSession, lsp::LanguageServer, transport};
use serde_json::{Value, json};
use std::{
    collections::BTreeMap,
    io,
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{
    io::{AsyncRead, AsyncWrite},
    sync::mpsc,
    task::JoinSet,
};

/// One connection owns its language session or debug target.
pub(crate) enum Server {
    Language(Box<LanguageServer>),
    Debug(Box<DebugSession>),
}

impl LanguageServer {
    pub async fn serve<R, W>(self, input: R, output: W) -> io::Result<()>
    where
        R: AsyncRead + Unpin + Send + 'static,
        W: AsyncWrite + Unpin + Send + 'static,
    {
        Server::Language(Box::new(self)).serve(input, output).await
    }
}
impl DebugSession {
    pub async fn serve<R, W>(self, input: R, output: W) -> io::Result<()>
    where
        R: AsyncRead + Unpin + Send + 'static,
        W: AsyncWrite + Unpin + Send + 'static,
    {
        Server::Debug(Box::new(self)).serve(input, output).await
    }
}

impl Server {
    /// Serve framed messages on caller-supplied streams. Cancellation stays on
    /// the reader task while the owner serially executes protocol requests.
    pub async fn serve<R, W>(mut self, input: R, mut output: W) -> io::Result<()>
    where
        R: AsyncRead + Unpin + Send + 'static,
        W: AsyncWrite + Unpin + Send + 'static,
    {
        let cancellations = Arc::new(Mutex::new(BTreeMap::<String, Cancellation>::new()));
        let (requests, mut incoming) = mpsc::channel(128);
        let mut tasks = JoinSet::new();
        let active = cancellations.clone();
        let reader = tasks.spawn(async move {
            let mut input = tokio::io::BufReader::new(input);
            let result = async {
                while let Some(message) = transport::read(&mut input).await? {
                    let canceled = if message["method"] == "$/cancelRequest" {
                        message["params"].get("id")
                    } else if message["command"] == "cancel" {
                        message["arguments"].get("requestId")
                    } else {
                        None
                    };
                    if let Some(id) = canceled {
                        if let Some(cancel) = active.lock().unwrap().get(&id.to_string()) {
                            cancel.cancel();
                        }
                        if message["method"] == "$/cancelRequest" {
                            continue;
                        }
                    }
                    let id = message
                        .get("id")
                        .or_else(|| message.get("seq"))
                        .map(Value::to_string);
                    let cancel = Cancellation::default();
                    if let Some(id) = &id {
                        use std::collections::btree_map::Entry;
                        match active.lock().unwrap().entry(id.clone()) {
                            Entry::Vacant(entry) => {
                                entry.insert(cancel.clone());
                            }
                            Entry::Occupied(_) => {
                                return Err(io::Error::other("duplicate active request ID"));
                            }
                        }
                    }
                    // The control reader must remain available under backpressure.
                    requests
                        .try_send((message, id, cancel))
                        .map_err(|_| io::Error::other("protocol request queue exhausted"))?;
                }
                Ok(())
            }
            .await;
            for cancel in active.lock().unwrap().values() {
                cancel.cancel();
            }
            result
        });
        let (outgoing, mut replies) = mpsc::channel(32);
        let active = cancellations.clone();
        tasks.spawn(async move {
            while let Some(message) = replies.recv().await {
                if let Err(error) = transport::write(&mut output, &message).await {
                    for cancel in active.lock().unwrap().values() {
                        cancel.cancel();
                    }
                    return Err(error);
                }
            }
            Ok(())
        });

        let mut sequence = 0_u64;
        let mut tick = tokio::time::interval(Duration::from_millis(5));
        let result = {
            let processing = async {
                loop {
                    tokio::select! {
                        request = incoming.recv() => {
                            let Some((message, id, cancel)) = request else { break; };
                            let exit = message["method"] == "exit";
                            match &mut self {
                                Self::Language(server) => {
                                    for reply in server.dispatch(message, &cancel).await {
                                        outgoing.send(reply).await.map_err(io::Error::other)?;
                                    }
                                }
                                Self::Debug(debug) => {
                                    let command = message["command"].as_str().unwrap_or("");
                                    let result = match command {
                                        "launch" => launch_debug(debug, &message["arguments"], &cancel).await,
                                        "cancel" => Ok(json!({})),
                                        _ => debug.request(command, &message["arguments"]).map_err(|error| error.to_string()),
                                    };
                                    sequence += 1;
                                    let mut reply = json!({
                                        "seq": sequence, "type": "response", "request_seq": message["seq"],
                                        "command": command, "success": result.is_ok(),
                                    });
                                    match result {
                                        Ok(body) => reply["body"] = body,
                                        Err(error) => reply["message"] = error.into(),
                                    }
                                    outgoing.send(reply).await.map_err(io::Error::other)?;
                                    if command == "initialize" {
                                        sequence += 1;
                                        outgoing.send(json!({"seq": sequence, "type": "event", "event": "initialized"}))
                                            .await.map_err(io::Error::other)?;
                                    }
                                }
                            }
                            if let Some(id) = id {
                                cancellations.lock().unwrap().remove(&id);
                            }
                            if exit { break; }
                        }
                        _ = tick.tick(), if matches!(self, Self::Debug(_)) => {
                            if let Self::Debug(debug) = &mut self {
                                debug.poll().map_err(io::Error::other)?;
                                for mut event in debug.events().map_err(io::Error::other)? {
                                    sequence += 1;
                                    event["seq"] = sequence.into();
                                    event["type"] = "event".into();
                                    outgoing.send(event).await.map_err(io::Error::other)?;
                                }
                            }
                        }
                    }
                }
                Ok(())
            };
            tokio::pin!(processing);
            // Monitor IO failures outside dispatch: a full response queue must
            // not prevent a reader failure from releasing this connection.
            loop {
                tokio::select! {
                    result = &mut processing => break result,
                    Some(completed) = tasks.join_next(), if !tasks.is_empty() => {
                        match completed {
                            Ok(Ok(())) => {}
                            Ok(Err(error)) => break Err(error),
                            Err(error) => break Err(io::Error::other(error)),
                        }
                    }
                }
            }
        };

        reader.abort();
        for cancel in cancellations.lock().unwrap().values() {
            cancel.cancel();
        }
        let close_result = match &mut self {
            Self::Language(server) => server.close().await.map_err(io::Error::other),
            Self::Debug(debug) => {
                debug.close();
                Ok(())
            }
        };
        drop(outgoing);
        let mut result = result.and(close_result);
        if result.is_err() {
            tasks.abort_all();
        }
        while let Some(completed) = tasks.join_next().await {
            match completed {
                Ok(task_result) => result = result.and(task_result),
                Err(error) if error.is_cancelled() => {}
                Err(error) => result = result.and(Err(io::Error::other(error))),
            }
        }
        result
    }
}

async fn launch_debug(
    debug: &mut DebugSession,
    args: &Value,
    cancel: &Cancellation,
) -> Result<Value, String> {
    let entry = args["entry"].as_str().unwrap_or("default");
    if args["workspace"].is_object() {
        let mut compiler = if let Some(path) = args["compilerImage"].as_str() {
            let image = tokio::fs::read(path)
                .await
                .map_err(|error| error.to_string())?;
            CompilerSession::new(&image).await
        } else {
            CompilerSession::bundled().await
        }
        .map_err(|error| error.to_string())?;
        let mut workspace = args["workspace"].clone();
        workspace["Operation"] = "workspace/open".into();
        compiler
            .call(workspace, cancel)
            .await
            .map_err(|error| error.to_string())?;
        let mut build = args.get("build").cloned().unwrap_or(json!({}));
        build["Revision"] = compiler.revision().into();
        build["Symbols"] = true.into();
        let result = compiler
            .call(
                json!({"Operation": "build/prepare", "Build": build}),
                cancel,
            )
            .await
            .map_err(|error| error.to_string())?;
        let image = result["ImageJSON"]
            .as_str()
            .filter(|image| !image.is_empty())
            .ok_or_else(|| format!("build diagnostics: {}", result["Diagnostics"]))?;
        debug
            .launch(
                image.as_bytes(),
                result["SymbolsJSON"].as_str(),
                serde_json::from_value(result["Sources"].clone())
                    .map_err(|error| error.to_string())?,
                entry,
            )
            .map_err(|error| error.to_string())?;
        compiler.close().await.map_err(|error| error.to_string())?;
    } else {
        let path = args["image"].as_str().ok_or("launch requires image path")?;
        let image = tokio::fs::read(path)
            .await
            .map_err(|error| error.to_string())?;
        let symbols = match args["symbols"].as_str() {
            Some(path) => Some(
                tokio::fs::read_to_string(path)
                    .await
                    .map_err(|error| error.to_string())?,
            ),
            None => None,
        };
        let sources = serde_json::from_value(args.get("sources").cloned().unwrap_or(json!({})))
            .map_err(|error| error.to_string())?;
        debug
            .launch(&image, symbols.as_deref(), sources, entry)
            .map_err(|error| error.to_string())?;
    }
    Ok(json!({}))
}

#[cfg(test)]
mod tests;
