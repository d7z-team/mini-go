use std::{
    hash::{Hash, Hasher},
    ops::{Deref, Range},
    sync::Arc,
};

/// Immutable guest bytes. Substrings share their backing allocation.
#[derive(Clone, Debug)]
pub struct ByteString {
    storage: Arc<[u8]>,
    start: usize,
    length: usize,
    // A decoded immutable string keeps its managed backing identity. Native
    // operators use the byte snapshot; storing the result reuses the backing.
    managed: Option<(u64, crate::heap::Handle, u64)>,
}

impl ByteString {
    pub(crate) fn from_managed(
        bytes: Vec<u8>,
        owner: u64,
        handle: crate::heap::Handle,
        offset: u64,
    ) -> Self {
        let mut value = Self::from(bytes);
        value.managed = Some((owner, handle, offset));
        value
    }

    pub(crate) fn managed_backing(&self) -> Option<(u64, crate::heap::Handle, u64)> {
        self.managed
            .map(|(owner, handle, offset)| (owner, handle, offset + self.start as u64))
    }

    pub(crate) fn slice(&self, range: Range<usize>) -> Self {
        assert!(range.start <= range.end && range.end <= self.length);
        Self {
            storage: self.storage.clone(),
            start: self.start + range.start,
            length: range.end - range.start,
            managed: self.managed,
        }
    }
}

impl Deref for ByteString {
    type Target = [u8];
    fn deref(&self) -> &[u8] {
        &self.storage[self.start..self.start + self.length]
    }
}

impl From<Arc<[u8]>> for ByteString {
    fn from(storage: Arc<[u8]>) -> Self {
        Self {
            length: storage.len(),
            storage,
            start: 0,
            managed: None,
        }
    }
}

impl From<Vec<u8>> for ByteString {
    fn from(bytes: Vec<u8>) -> Self {
        Self::from(Arc::<[u8]>::from(bytes))
    }
}

impl From<&[u8]> for ByteString {
    fn from(bytes: &[u8]) -> Self {
        Self::from(Arc::<[u8]>::from(bytes))
    }
}

impl PartialEq for ByteString {
    fn eq(&self, other: &Self) -> bool {
        **self == **other
    }
}
impl Eq for ByteString {}
impl PartialOrd for ByteString {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}
impl Ord for ByteString {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        self.deref().cmp(other.deref())
    }
}
impl Hash for ByteString {
    fn hash<H: Hasher>(&self, state: &mut H) {
        self.deref().hash(state);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nested_binary_substrings_share_storage_and_compare_visible_bytes() {
        let original = ByteString::from(&b"prefix\xff\x00answer suffix"[..]);
        let part = original.slice(6..14);
        let nested = part.slice(2..8);
        assert!(Arc::ptr_eq(&original.storage, &nested.storage));
        assert_eq!(&*part, b"\xff\x00answer");
        assert_eq!(nested, ByteString::from(&b"answer"[..]));
        drop(original);
        drop(part);
        assert_eq!(&*nested, b"answer");
    }

    #[test]
    fn rune_decoding_handles_all_byte_pairs_and_truncated_sequences() {
        for first in 0..=255_u8 {
            for second in 0..=255_u8 {
                let bytes = [first, second, b'a', b'b', b'c'];
                let expected = match std::str::from_utf8(&bytes) {
                    Ok(text) => text.chars().next(),
                    Err(error) => std::str::from_utf8(&bytes[..error.valid_up_to()])
                        .unwrap()
                        .chars()
                        .next(),
                }
                .map_or((0xfffd, 1), |rune| (rune as u32, rune.len_utf8()));
                assert_eq!(crate::value::decode_rune(&bytes), expected);
            }
        }
        for text in ["a", "é", "界", "😀", "\u{10ffff}"] {
            let rune = text.chars().next().unwrap();
            assert_eq!(
                crate::value::decode_rune(text.as_bytes()),
                (rune as u32, text.len())
            );
            for length in 1..text.len() {
                assert_eq!(
                    crate::value::decode_rune(&text.as_bytes()[..length]),
                    (0xfffd, 1)
                );
            }
        }
    }
}
