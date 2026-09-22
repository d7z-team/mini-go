use crate::transport;
use serde_json::json;
use tokio::io::AsyncWriteExt;

#[tokio::test]
async fn fragmented_messages_and_clean_eof() {
    let (mut sender, mut receiver) = tokio::io::duplex(64);
    let input = json!({"method":"hello","params":"你好"});
    let mut frame = Vec::new();
    transport::write(&mut frame, &input).await.unwrap();
    let task = tokio::spawn(async move {
        for byte in frame {
            sender.write_all(&[byte]).await.unwrap();
        }
    });
    assert_eq!(transport::read(&mut receiver).await.unwrap(), Some(input));
    assert!(transport::read(&mut receiver).await.unwrap().is_none());
    task.await.unwrap();
}

#[tokio::test]
async fn invalid_frames_fail_without_accepting_a_partial_message() {
    for bytes in [
        b"Content-Length: 1\r\nContent-Length: 1\r\n\r\n0".as_slice(),
        b"Content-Length: 67108865\r\n\r\n",
        b"Content-Length: -1\r\n\r\n",
        b"Content-Length: 2\r\n\r\n0",
        b"Content-Length: 1\r\n\r\nx",
        b"Content-Length: 1\r\n",
    ] {
        assert!(transport::read(&mut &bytes[..]).await.is_err(), "{bytes:?}");
    }
}
