//! Map storage keeps value copies and a private hash index under the heap owner.

use super::{Data, Value};
use crate::{error::RuntimeError, types::TypeRegistry};
use std::{
    collections::{HashMap, hash_map::RandomState},
    hash::{BuildHasher, Hash, Hasher},
    ops::Deref,
};

#[derive(Clone, Debug, Default)]
pub struct MapStorage {
    entries: Vec<(Value, Value)>,
    hashes: Vec<u64>,
    buckets: HashMap<u64, Vec<usize>>,
    hasher: RandomState,
    identities: Box<EntryIdentities>,
}

#[derive(Clone, Debug, Default)]
struct EntryIdentities {
    ids: Vec<u64>,
    positions: HashMap<u64, usize>,
    next_id: u64,
}

struct MapKey<'a>(&'a Value);

impl Hash for MapKey<'_> {
    fn hash<H: Hasher>(&self, state: &mut H) {
        let mut pending = Vec::new();
        let mut value = self.0;
        loop {
            // Interface boxing and type aliases do not change the hash. The
            // equality check still distinguishes dynamic named identities.
            if let Data::Interface(inner) = &value.data {
                value = inner;
                continue;
            }
            std::mem::discriminant(&value.data).hash(state);
            match &value.data {
                Data::Nil => {}
                Data::Bool(value) => value.hash(state),
                Data::Integer(value) => value.hash(state),
                Data::Unsigned(value) => value.hash(state),
                Data::Float(value) => (if *value == 0.0 { 0 } else { value.to_bits() }).hash(state),
                Data::Complex { real, imag } => {
                    (if *real == 0.0 { 0 } else { real.to_bits() }).hash(state);
                    (if *imag == 0.0 { 0 } else { imag.to_bits() }).hash(state);
                }
                Data::String(value) => value.hash(state),
                Data::Pointer(address) => address.hash(state),
                Data::ResourceRef(handle) => handle.hash(state),
                Data::Array(values) => {
                    values.len().hash(state);
                    pending.extend(values.iter());
                }
                Data::Struct(fields) => {
                    fields.len().hash(state);
                    for (name, value) in fields {
                        name.hash(state);
                        pending.push(value);
                    }
                }
                _ => unreachable!("map keys are checked for comparability before indexing"),
            }
            let Some(next) = pending.pop() else { break };
            value = next;
        }
    }
}

impl MapStorage {
    pub(crate) fn cleared(&self) -> Self {
        Self {
            identities: Box::new(EntryIdentities {
                next_id: self.identities.next_id,
                ..Default::default()
            }),
            ..Self::default()
        }
    }
    pub(crate) fn entry_ids(&self) -> &[u64] {
        &self.identities.ids
    }
    pub(crate) fn entry_by_id(&self, id: u64) -> Option<&(Value, Value)> {
        self.identities
            .positions
            .get(&id)
            .map(|index| &self.entries[*index])
    }
    pub(crate) fn find(
        &self,
        key: &Value,
        types: &TypeRegistry,
    ) -> Result<Option<usize>, RuntimeError> {
        self.find_hashed(key, self.key_hash(key), types)
    }

    pub(crate) fn key_hash(&self, key: &Value) -> u64 {
        self.hasher.hash_one(MapKey(key))
    }

    pub(crate) fn find_hashed(
        &self,
        key: &Value,
        hash: u64,
        types: &TypeRegistry,
    ) -> Result<Option<usize>, RuntimeError> {
        if let Some(indexes) = self.buckets.get(&hash) {
            for index in indexes {
                if crate::operators::equal(&self.entries[*index].0, key, types)? {
                    return Ok(Some(*index));
                }
            }
        }
        Ok(None)
    }

    pub(crate) fn insert(&mut self, key: Value, value: Value) {
        let hash = self.key_hash(&key);
        self.insert_hashed(key, value, hash);
    }

    pub(crate) fn insert_hashed(&mut self, key: Value, value: Value, hash: u64) {
        self.identities.next_id += 1;
        self.identities.ids.push(self.identities.next_id);
        self.identities
            .positions
            .insert(self.identities.next_id, self.entries.len());
        self.buckets
            .entry(hash)
            .or_default()
            .push(self.entries.len());
        self.hashes.push(hash);
        self.entries.push((key, value));
    }

    pub(crate) fn set_value(&mut self, index: usize, value: Value) {
        self.entries[index].1 = value;
    }

    pub(crate) fn remove(&mut self, index: usize) {
        self.identities
            .positions
            .remove(&self.identities.ids.swap_remove(index));
        if index < self.identities.ids.len() {
            self.identities
                .positions
                .insert(self.identities.ids[index], index);
        }
        let hash = self.hashes[index];
        let bucket = self.buckets.get_mut(&hash).unwrap();
        bucket.retain(|candidate| *candidate != index);
        if bucket.is_empty() {
            self.buckets.remove(&hash);
        }
        self.entries.swap_remove(index);
        self.hashes.swap_remove(index);
        if index < self.entries.len() {
            let previous = self.entries.len();
            for candidate in self.buckets.get_mut(&self.hashes[index]).unwrap() {
                if *candidate == previous {
                    *candidate = index;
                    break;
                }
            }
        }
    }
}

impl Deref for MapStorage {
    type Target = [(Value, Value)];
    fn deref(&self) -> &Self::Target {
        &self.entries
    }
}

impl<'a> IntoIterator for &'a MapStorage {
    type Item = &'a (Value, Value);
    type IntoIter = std::slice::Iter<'a, (Value, Value)>;
    fn into_iter(self) -> Self::IntoIter {
        self.entries.iter()
    }
}

impl IntoIterator for MapStorage {
    type Item = (Value, Value);
    type IntoIter = std::vec::IntoIter<Self::Item>;
    fn into_iter(self) -> Self::IntoIter {
        self.entries.into_iter()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{contract_generated as wire, types::TypeIdentity};
    use std::collections::BTreeMap;

    #[test]
    fn indexed_updates_and_deletions_match_a_value_map() {
        let types = TypeRegistry::new([]).unwrap();
        let mut storage = MapStorage::default();
        let mut expected = BTreeMap::new();
        let mut seed = 0x13de_7162_918a_4455_u64;
        for step in 0..20_000 {
            seed ^= seed << 13;
            seed ^= seed >> 7;
            seed ^= seed << 17;
            let key = (seed % 2048) as i64;
            let position = storage.find(&Value::int(key), &types).unwrap();
            if seed.is_multiple_of(3) {
                if let Some(index) = position {
                    storage.remove(index);
                }
                expected.remove(&key);
            } else {
                if let Some(index) = position {
                    storage.set_value(index, Value::int(step));
                } else {
                    storage.insert(Value::int(key), Value::int(step));
                }
                expected.insert(key, step);
            }
            assert_eq!(storage.len(), expected.len());
            if step % 256 == 0 {
                for (key, expected) in &expected {
                    let index = storage.find(&Value::int(*key), &types).unwrap().unwrap();
                    assert_eq!(storage[index].1.integer().unwrap(), *expected);
                }
            }
        }
    }

    #[test]
    fn entry_identity_survives_update_and_move_but_not_reinsertion() {
        let mut map = MapStorage::default();
        map.insert(Value::int(1), Value::int(10));
        map.insert(Value::int(2), Value::int(20));
        let ids = map.entry_ids().to_vec();
        map.set_value(1, Value::int(42));
        map.remove(0);
        assert!(map.entry_by_id(ids[0]).is_none());
        assert_eq!(map.entry_by_id(ids[1]).unwrap().1.integer().unwrap(), 42);
        map.insert(Value::int(1), Value::int(30));
        assert!(map.entry_by_id(ids[0]).is_none());
        map = map.cleared();
        map.insert(Value::int(2), Value::int(99));
        assert!(map.entry_by_id(ids[1]).is_none());
    }

    #[test]
    fn key_hashing_preserves_ieee_zero_nan_and_dynamic_named_identity() {
        let artifact: wire::Artifact = serde_json::from_str(r#"{
            "module":{"path":"test"},"type_table":{"nodes":[
                {"id":"A","kind":4,"name":"A","identity":{"module_path":"test","decl_id":"A"},"underlying":{"kind":3,"primitive":3}},
                {"id":"B","kind":4,"name":"B","identity":{"module_path":"test","decl_id":"B"},"underlying":{"kind":3,"primitive":3}}
            ]}
        }"#).unwrap();
        let types = TypeRegistry::new([&artifact]).unwrap();
        let float = |number| Value {
            typ: TypeIdentity::Primitive(wire::PrimitiveFloat64),
            data: Data::Float(number),
        };
        let mut storage = MapStorage::default();
        storage.insert(float(-0.0), Value::int(20));
        let index = storage.find(&float(0.0), &types).unwrap().unwrap();
        assert_eq!(storage[index].1.integer().unwrap(), 20);
        for _ in 0..3 {
            storage.insert(float(f64::NAN), Value::int(1));
        }
        assert!(storage.find(&float(f64::NAN), &types).unwrap().is_none());
        assert_eq!(storage.len(), 4);
        let named = |name: &str| Value {
            typ: TypeIdentity::Any,
            data: Data::Interface(std::sync::Arc::new(Value {
                typ: TypeIdentity::Named(std::sync::Arc::new(wire::TypeKey {
                    module_path: "test".into(),
                    decl_id: name.into(),
                })),
                data: Data::Integer(42),
            })),
        };
        storage.insert(named("A"), Value::int(1));
        storage.insert(named("B"), Value::int(2));
        for (name, expected) in [("A", 1), ("B", 2)] {
            let index = storage.find(&named(name), &types).unwrap().unwrap();
            assert_eq!(storage[index].1.integer().unwrap(), expected);
        }
        storage.remove(0);
        assert!(storage.find(&float(0.0), &types).unwrap().is_none());
        assert!(storage.find(&named("B"), &types).unwrap().is_some());
    }
}
