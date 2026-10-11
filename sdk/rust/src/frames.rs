//! The capsule control and link channels: a 4-byte big-endian length, then a CBOR map
//! `{t, id, b}` (Capsules spec, Framing). Frames over 1 MiB are refused.

use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;

const MAX: usize = 1 << 20;

#[derive(Debug, Serialize, Deserialize)]
pub struct Frame {
    pub t: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub b: Option<Value>,
}

pub struct Conn(UnixStream);

impl Conn {
    pub async fn connect(path: &str) -> std::io::Result<Self> {
        Ok(Conn(UnixStream::connect(path).await?))
    }

    pub async fn send(&mut self, t: &str, id: u64, b: Option<Value>) -> std::io::Result<()> {
        let mut body = Vec::new();
        ciborium::into_writer(&Frame { t: t.into(), id: (id != 0).then_some(id), b }, &mut body)
            .map_err(|e| std::io::Error::other(e.to_string()))?;
        let mut out = (body.len() as u32).to_be_bytes().to_vec();
        out.extend(body);
        self.0.write_all(&out).await
    }

    pub async fn recv(&mut self) -> std::io::Result<Frame> {
        let mut n = [0u8; 4];
        self.0.read_exact(&mut n).await?;
        let n = u32::from_be_bytes(n) as usize;
        if n > MAX {
            return Err(std::io::Error::other("frame over 1 MiB"));
        }
        let mut b = vec![0; n];
        self.0.read_exact(&mut b).await?;
        ciborium::from_reader(&b[..]).map_err(|e| std::io::Error::other(e.to_string()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn frame_round_trip() {
        let f = Frame { t: "call".into(), id: Some(7), b: Some(serde_json::json!({"method": "spend", "body": {"amount": -3}})) };
        let mut buf = Vec::new();
        ciborium::into_writer(&f, &mut buf).unwrap();
        let back: Frame = ciborium::from_reader(&buf[..]).unwrap();
        assert_eq!((back.t.as_str(), back.id), ("call", Some(7)));
        assert_eq!(back.b.unwrap()["body"]["amount"], -3);
    }
}
