//! Bounded Content-Length framing shared by LSP and DAP.
use std::io;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
pub const MAX_MESSAGE: usize = 64 << 20;
pub async fn read<R: AsyncRead + Unpin>(reader: &mut R) -> io::Result<Option<serde_json::Value>> {
    let mut header = Vec::new();
    loop {
        let mut byte = [0];
        let count = reader.read(&mut byte).await?;
        if count == 0 {
            return if header.is_empty() {
                Ok(None)
            } else {
                Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "partial protocol header",
                ))
            };
        }
        header.push(byte[0]);
        if header.len() > 8192 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "protocol header too large",
            ));
        }
        if header.ends_with(b"\r\n\r\n") {
            break;
        }
    }
    let header = std::str::from_utf8(&header)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))?;
    let mut length = None;
    for line in header.split("\r\n").filter(|line| !line.is_empty()) {
        let (name, value) = line
            .split_once(':')
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "invalid protocol header"))?;
        if name.eq_ignore_ascii_case("Content-Length") {
            if length.is_some() {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "duplicate Content-Length",
                ));
            }
            let value = value.trim();
            if value.is_empty() || !value.bytes().all(|byte| byte.is_ascii_digit()) {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    "invalid Content-Length",
                ));
            }
            length = Some(
                value
                    .parse::<usize>()
                    .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))?,
            );
        }
    }
    let length = length
        .filter(|length| *length > 0 && *length <= MAX_MESSAGE)
        .ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                "missing or excessive Content-Length",
            )
        })?;
    let mut body = vec![0; length];
    reader.read_exact(&mut body).await?;
    serde_json::from_slice(&body)
        .map(Some)
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))
}
pub async fn write<W: AsyncWrite + Unpin>(
    writer: &mut W,
    message: &serde_json::Value,
) -> io::Result<()> {
    let bytes = serde_json::to_vec(message)?;
    if bytes.len() > MAX_MESSAGE {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "protocol response too large",
        ));
    }
    writer
        .write_all(format!("Content-Length: {}\r\n\r\n", bytes.len()).as_bytes())
        .await?;
    writer.write_all(&bytes).await?;
    writer.flush().await
}

#[cfg(test)]
mod tests;
