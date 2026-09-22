use mini_go::ffi::Cancellation;
use mini_go::lsp::LanguageServer;
use serde_json::json;

#[tokio::test]
async fn initialization_language_requests_and_shutdown_follow_protocol_state() {
    let workspace =
        serde_json::from_str(include_str!("../../../testdata/language/workspace.json")).unwrap();
    let mut server = LanguageServer::new(include_bytes!("../assets/compiler.json.gz"), workspace)
        .await
        .unwrap();
    let cancel = Cancellation::default();
    let before = server
        .dispatch(
            json!({"id":1,"method":"textDocument/hover","params":{}}),
            &cancel,
        )
        .await;
    assert_eq!(before[0]["error"]["code"], -32002);
    let initialized = server
        .dispatch(json!({"id":2,"method":"initialize","params":{}}), &cancel)
        .await;
    assert_eq!(
        initialized[0]["result"]["capabilities"]["positionEncoding"],
        "utf-16"
    );
    let hover=server.dispatch(json!({"id":3,"method":"textDocument/hover","params":{"textDocument":{"uri":"mini-go://sample/main.mgo"},"position":{"line":2,"character":6}}}),&cancel).await;
    assert!(
        hover[0]["result"]["contents"]["value"]
            .as_str()
            .unwrap()
            .contains("Answer")
    );
    let unknown = server
        .dispatch(json!({"id":4,"method":"unknown"}), &cancel)
        .await;
    assert_eq!(unknown[0]["error"]["code"], -32601);
    let opened = server.dispatch(json!({"method":"textDocument/didOpen","params":{"textDocument":{"uri":"mini-go://sample/main.mgo","version":1,"text":"package sample\nfunc Saved() int { return 7 }\n"}}}), &cancel).await;
    assert!(
        opened
            .iter()
            .all(|message| message["method"] != "window/logMessage"),
        "{opened:?}"
    );
    let saved = server.dispatch(json!({"method":"textDocument/didSave","params":{"textDocument":{"uri":"mini-go://sample/main.mgo"}}}), &cancel).await;
    assert!(
        saved
            .iter()
            .all(|message| message["method"] != "window/logMessage"),
        "{saved:?}"
    );
    server.dispatch(json!({"method":"textDocument/didClose","params":{"textDocument":{"uri":"mini-go://sample/main.mgo"}}}), &cancel).await;
    let symbols = server.dispatch(json!({"id":6,"method":"textDocument/documentSymbol","params":{"textDocument":{"uri":"mini-go://sample/main.mgo"}}}), &cancel).await;
    assert!(
        symbols[0]["result"]
            .as_array()
            .unwrap()
            .iter()
            .any(|symbol| symbol["name"] == "Saved" && symbol["kind"] == 12),
        "{symbols:?}"
    );
    let shutdown = server
        .dispatch(json!({"id":5,"method":"shutdown"}), &cancel)
        .await;
    assert!(shutdown[0]["result"].is_null());
    assert!(
        server
            .dispatch(json!({"method":"exit"}), &cancel)
            .await
            .is_empty()
    );
}
