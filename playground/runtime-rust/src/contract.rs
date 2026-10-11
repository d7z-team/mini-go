//! Canonical serialization for the current Go-produced runtime contract.

use serde::{Deserialize, Serialize, de::DeserializeOwned};
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, HashSet};
use std::io::{self, BufWriter, Write};
use std::ops::{Deref, DerefMut};

struct UniqueJson;

impl<'de> Deserialize<'de> for UniqueJson {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        struct Visitor;
        impl<'de> serde::de::Visitor<'de> for Visitor {
            type Value = UniqueJson;

            fn expecting(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                formatter.write_str("a JSON value without duplicate object keys")
            }

            fn visit_bool<E: serde::de::Error>(self, _: bool) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_i64<E: serde::de::Error>(self, _: i64) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_u64<E: serde::de::Error>(self, _: u64) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_f64<E: serde::de::Error>(self, _: f64) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_str<E: serde::de::Error>(self, _: &str) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_string<E: serde::de::Error>(self, _: String) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_none<E: serde::de::Error>(self) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_unit<E: serde::de::Error>(self) -> Result<Self::Value, E> {
                Ok(UniqueJson)
            }
            fn visit_some<D: serde::Deserializer<'de>>(
                self,
                value: D,
            ) -> Result<Self::Value, D::Error> {
                UniqueJson::deserialize(value)
            }
            fn visit_seq<A: serde::de::SeqAccess<'de>>(
                self,
                mut sequence: A,
            ) -> Result<Self::Value, A::Error> {
                while sequence.next_element::<UniqueJson>()?.is_some() {}
                Ok(UniqueJson)
            }
            fn visit_map<A: serde::de::MapAccess<'de>>(
                self,
                mut map: A,
            ) -> Result<Self::Value, A::Error> {
                let mut seen = HashSet::new();
                while let Some(key) = map.next_key::<String>()? {
                    if !seen.insert(key.clone()) {
                        return Err(serde::de::Error::custom(format!(
                            "duplicate object key {key:?}"
                        )));
                    }
                    map.next_value::<UniqueJson>()?;
                }
                Ok(UniqueJson)
            }
        }
        deserializer.deserialize_any(Visitor)
    }
}

/// Decodes the current wire schema with exact field names and globally unique
/// object keys. Generated models additionally reject unknown and duplicate fields.
pub fn strict_json_from_slice<T: DeserializeOwned>(bytes: &[u8]) -> Result<T, serde_json::Error> {
    serde_json::from_slice::<UniqueJson>(bytes)?;
    serde_json::from_slice(bytes)
}

pub fn strict_json_from_str<T: DeserializeOwned>(text: &str) -> Result<T, serde_json::Error> {
    strict_json_from_slice(text.as_bytes())
}

/// Go's wire contract distinguishes nil slices from allocated empty slices.
#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(transparent)]
pub struct GoSlice<T>(pub Option<Vec<T>>);

impl<T> Default for GoSlice<T> {
    fn default() -> Self {
        Self(None)
    }
}

impl<T> Deref for GoSlice<T> {
    type Target = [T];
    fn deref(&self) -> &[T] {
        self.0.as_deref().unwrap_or_default()
    }
}

impl<T> DerefMut for GoSlice<T> {
    fn deref_mut(&mut self) -> &mut [T] {
        self.0.get_or_insert_with(Vec::new).as_mut_slice()
    }
}

impl<T> GoSlice<T> {
    pub fn is_empty(&self) -> bool {
        self.deref().is_empty()
    }
}

/// Applies Go's omitempty rule to a nullable map.
pub fn map_is_empty<T>(value: &Option<BTreeMap<String, T>>) -> bool {
    value.as_ref().is_none_or(BTreeMap::is_empty)
}

pub(crate) fn is_default<T: Default + PartialEq>(value: &T) -> bool {
    value == &T::default()
}

struct CanonicalFormatter;

impl serde_json::ser::Formatter for CanonicalFormatter {
    fn write_string_fragment<W: std::io::Write + ?Sized>(
        &mut self,
        writer: &mut W,
        fragment: &str,
    ) -> std::io::Result<()> {
        let mut start = 0;
        for (index, character) in fragment.char_indices() {
            let escape = match character {
                '\u{2028}' => b"\\u2028",
                '\u{2029}' => b"\\u2029",
                _ => continue,
            };
            writer.write_all(&fragment.as_bytes()[start..index])?;
            writer.write_all(escape)?;
            start = index + character.len_utf8();
        }
        writer.write_all(&fragment.as_bytes()[start..])
    }
}

/// Encodes fields in declaration order and maps in lexical key order.
/// Go 1.26 escapes line separators in strings, while preserving their original
/// spelling inside RawMessage when EscapeHTML is false.
pub fn canonical_json(value: &impl Serialize) -> Result<Vec<u8>, serde_json::Error> {
    let mut output = CanonicalWriter::new(Vec::new());
    value.serialize(&mut serde_json::Serializer::with_formatter(
        &mut output,
        CanonicalFormatter,
    ))?;
    Ok(output.sink)
}

/// Returns SHA-256 over canonical wire bytes, without a framing newline.
pub fn canonical_hash(value: &impl Serialize) -> Result<String, serde_json::Error> {
    // Buffer serializer fragments so small fields do not each update SHA-256.
    let mut output = CanonicalWriter::new(BufWriter::new(DigestWriter(Sha256::new())));
    value.serialize(&mut serde_json::Serializer::with_formatter(
        &mut output,
        CanonicalFormatter,
    ))?;
    let digest = output
        .sink
        .into_inner()
        .map_err(|error| serde_json::Error::io(error.into_error()))?;
    Ok(format!("{:x}", digest.0.finalize()))
}

// RawValue preserves number spelling and object order. Go compacts whitespace
// outside strings, including when a quoted fragment spans serializer writes.
struct CanonicalWriter<W> {
    sink: W,
    quoted: bool,
    escaped: bool,
}

impl<W> CanonicalWriter<W> {
    fn new(sink: W) -> Self {
        Self {
            sink,
            quoted: false,
            escaped: false,
        }
    }
}

impl<W: Write> Write for CanonicalWriter<W> {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        let mut start = 0;
        for (index, &byte) in bytes.iter().enumerate() {
            if self.quoted {
                if self.escaped {
                    self.escaped = false;
                } else if byte == b'\\' {
                    self.escaped = true;
                } else if byte == b'"' {
                    self.quoted = false;
                }
            } else if byte == b'"' {
                self.quoted = true;
            } else if matches!(byte, b' ' | b'\t' | b'\r' | b'\n') {
                self.sink.write_all(&bytes[start..index])?;
                start = index + 1;
            }
        }
        self.sink.write_all(&bytes[start..])?;
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        self.sink.flush()
    }
}

struct DigestWriter(Sha256);

impl Write for DigestWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.0.update(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_writer_preserves_strings_across_every_split() {
        let input = r#" { "key" : "a b\\\" c", "number" : -0.12500e+9, "array" : [null, true], "unicode" : "界  " } "#;
        let expected =
            r#"{"key":"a b\\\" c","number":-0.12500e+9,"array":[null,true],"unicode":"界  "}"#;
        let raw: Box<serde_json::value::RawValue> = serde_json::from_str(input).unwrap();
        assert_eq!(canonical_json(&raw).unwrap(), expected.as_bytes());
        for split in 0..=input.len() {
            let mut writer = CanonicalWriter::new(Vec::new());
            writer.write_all(&input.as_bytes()[..split]).unwrap();
            writer.write_all(&input.as_bytes()[split..]).unwrap();
            writer.flush().unwrap();
            assert_eq!(writer.sink, expected.as_bytes(), "split {split}");
        }
        let mut writer = CanonicalWriter::new(Vec::new());
        for byte in input.as_bytes() {
            writer.write_all(&[*byte]).unwrap();
        }
        assert_eq!(writer.sink, expected.as_bytes());
    }

    #[test]
    fn streaming_hash_matches_canonical_bytes_across_buffer_boundaries() {
        #[derive(Serialize)]
        struct Envelope {
            text: String,
            raw: Box<serde_json::value::RawValue>,
        }
        let value = Envelope {
            text: "quotes\" and slash\\ and \u{2028}\u{2029}".repeat(2048),
            raw: serde_json::value::RawValue::from_string(format!(
                " {{ \"raw\" : {}, \"number\" : 1.000e+12 }} ",
                serde_json::to_string(&" \\".repeat(8192)).unwrap()
            ))
            .unwrap(),
        };
        let bytes = canonical_json(&value).unwrap();
        assert_eq!(
            canonical_hash(&value).unwrap(),
            format!("{:x}", Sha256::digest(&bytes))
        );
        let decoded: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(decoded["text"], value.text);
        assert!(String::from_utf8(bytes).unwrap().contains("\\u2028\\u2029"));
    }

    #[test]
    fn canonical_writer_handles_short_writes_and_propagates_failures() {
        struct Sink {
            bytes: Vec<u8>,
            remaining: usize,
        }
        impl Write for Sink {
            fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
                if bytes.is_empty() {
                    return Ok(0);
                }
                if self.remaining == 0 {
                    return Err(io::Error::other("injected write failure"));
                }
                let size = bytes.len().min(3).min(self.remaining);
                self.bytes.extend_from_slice(&bytes[..size]);
                self.remaining -= size;
                Ok(size)
            }
            fn flush(&mut self) -> io::Result<()> {
                Err(io::Error::other("injected flush failure"))
            }
        }
        let input = r#" { "text" : "keep spaces" } "#;
        let mut writer = CanonicalWriter::new(Sink {
            bytes: Vec::new(),
            remaining: 100,
        });
        writer.write_all(input.as_bytes()).unwrap();
        assert_eq!(writer.sink.bytes, br#"{"text":"keep spaces"}"#);
        assert!(
            writer
                .flush()
                .unwrap_err()
                .to_string()
                .contains("flush failure")
        );
        let mut writer = CanonicalWriter::new(Sink {
            bytes: Vec::new(),
            remaining: 4,
        });
        let error = serde_json::json!({"text":"keep spaces"})
            .serialize(&mut serde_json::Serializer::with_formatter(
                &mut writer,
                CanonicalFormatter,
            ))
            .unwrap_err();
        assert!(error.is_io());
        assert!(error.to_string().contains("write failure"));
    }
}
