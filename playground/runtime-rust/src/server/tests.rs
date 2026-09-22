use crate::{lsp::LanguageServer, server::Server, transport};
use serde_json::json;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

#[tokio::test]
async fn dap_connection_preserves_response_event_order_and_closes_on_eof() {
    let (client, stream) = tokio::io::duplex(8192);
    let (input, output) = tokio::io::split(stream);
    let server = tokio::spawn(Server::Debug(Box::default()).serve(input, output));
    let (mut reader, mut writer) = tokio::io::split(client);
    let mut pipeline = Vec::new();
    transport::write(&mut pipeline, &json!({"seq": 1, "command": "initialize"}))
        .await
        .unwrap();
    transport::write(&mut pipeline, &json!({"seq": 2, "command": "unknown"}))
        .await
        .unwrap();
    writer.write_all(&pipeline).await.unwrap();
    let response = transport::read(&mut reader).await.unwrap().unwrap();
    assert_eq!(response["request_seq"], 1);
    assert_eq!(response["success"], true);
    let initialized = transport::read(&mut reader).await.unwrap().unwrap();
    assert_eq!(initialized["event"], "initialized");
    assert!(initialized["seq"].as_u64() > response["seq"].as_u64());
    let response = transport::read(&mut reader).await.unwrap().unwrap();
    assert_eq!(response["request_seq"], 2);
    assert_eq!(response["success"], false);
    writer.shutdown().await.unwrap();
    tokio::time::timeout(Duration::from_secs(2), server)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(transport::read(&mut reader).await.unwrap().is_none());
}

#[tokio::test]
async fn failed_output_releases_the_connection_reader() {
    let (mut client, input) = tokio::io::duplex(8192);
    let (output, receiver) = tokio::io::duplex(8192);
    drop(receiver);
    let server = tokio::spawn(Server::Debug(Box::default()).serve(input, output));
    transport::write(&mut client, &json!({"seq": 1, "command": "initialize"}))
        .await
        .unwrap();
    assert!(
        tokio::time::timeout(Duration::from_secs(2), server)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    assert_eq!(client.read(&mut [0]).await.unwrap(), 0);
}

#[tokio::test]
async fn malformed_input_cancels_a_backpressured_writer() {
    let (mut client, input) = tokio::io::duplex(8192);
    let (output, _unread) = tokio::io::duplex(1);
    let server = tokio::spawn(Server::Debug(Box::default()).serve(input, output));
    for sequence in 1..=40 {
        transport::write(
            &mut client,
            &json!({"seq": sequence, "command": "initialize"}),
        )
        .await
        .unwrap();
        tokio::task::yield_now().await;
    }
    client.write_all(b"invalid header\r\n\r\n").await.unwrap();
    assert!(
        tokio::time::timeout(Duration::from_secs(2), server)
            .await
            .unwrap()
            .unwrap()
            .is_err()
    );
    assert_eq!(client.read(&mut [0]).await.unwrap(), 0);
}

#[tokio::test]
async fn lsp_control_reader_cancels_a_request_and_keeps_the_connection_usable() {
    let workspace =
        serde_json::from_str(include_str!("../../../../testdata/language/workspace.json")).unwrap();
    let language = LanguageServer::new(include_bytes!("../../assets/compiler.json.gz"), workspace)
        .await
        .unwrap();
    let (client, stream) = tokio::io::duplex(8192);
    let (input, output) = tokio::io::split(stream);
    let server = tokio::spawn(Server::Language(Box::new(language)).serve(input, output));
    let (mut reader, mut writer) = tokio::io::split(client);
    let mut messages = Vec::new();
    transport::write(
        &mut messages,
        &json!({"id": 1, "method": "initialize", "params": {}}),
    )
    .await
    .unwrap();
    transport::write(
        &mut messages,
        &json!({"method": "$/cancelRequest", "params": {"id": 1}}),
    )
    .await
    .unwrap();
    writer.write_all(&messages).await.unwrap();
    let canceled = transport::read(&mut reader).await.unwrap().unwrap();
    assert_eq!(canceled["error"]["code"], -32800, "{canceled}");
    transport::write(
        &mut writer,
        &json!({"id": 2, "method": "initialize", "params": {}}),
    )
    .await
    .unwrap();
    let initialized = transport::read(&mut reader).await.unwrap().unwrap();
    assert_eq!(
        initialized["result"]["capabilities"]["positionEncoding"], "utf-16",
        "{initialized}"
    );
    transport::write(&mut writer, &json!({"method": "exit"}))
        .await
        .unwrap();
    tokio::time::timeout(Duration::from_secs(2), server)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(transport::read(&mut reader).await.unwrap().is_none());
}
