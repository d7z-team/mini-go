//! Struct value copies share immutable fields until a destination is written.

use super::Value;
use std::{
    collections::BTreeMap,
    ops::Index,
    sync::{Arc, OnceLock},
};

#[derive(Clone, Debug)]
pub struct StructStorage {
    identity: Arc<()>,
    names: Arc<Vec<String>>,
    fields: Arc<StructFields>,
}

#[derive(Clone, Debug)]
struct StructFields {
    values: Vec<Value>,
    initialized: Vec<bool>,
    logical_bytes: OnceLock<u64>,
}

impl StructStorage {
    pub(crate) fn with_layout(
        names: Arc<Vec<String>>,
        values: Vec<Value>,
        initialized: Vec<bool>,
    ) -> Self {
        debug_assert_eq!(names.len(), values.len());
        debug_assert_eq!(names.len(), initialized.len());
        Self {
            identity: Arc::default(),
            names,
            fields: Arc::new(StructFields {
                values,
                initialized,
                logical_bytes: OnceLock::new(),
            }),
        }
    }
    pub(crate) fn identity(&self) -> usize {
        Arc::as_ptr(&self.identity) as usize
    }
    pub(crate) fn fresh_zero(&self) -> Self {
        Self {
            identity: Arc::default(),
            names: self.names.clone(),
            fields: self.fields.clone(),
        }
    }
    pub(crate) fn zero(values: BTreeMap<String, Value>) -> Self {
        let mut storage = Self::from(values);
        Arc::get_mut(&mut storage.fields)
            .unwrap()
            .initialized
            .fill(false);
        storage
    }
    pub(crate) fn initialized_values(&self) -> impl Iterator<Item = &Value> {
        self.fields
            .values
            .iter()
            .zip(&self.fields.initialized)
            .filter_map(|(value, initialized)| initialized.then_some(value))
    }
    pub(crate) fn guest_slots(&self) -> usize {
        if !self
            .fields
            .initialized
            .iter()
            .any(|initialized| *initialized)
        {
            0
        } else {
            self.fields.values.len()
        }
    }
    pub fn get_mut(&mut self, name: &str) -> Option<&mut Value> {
        let index = self.field_index(name)?;
        if Arc::strong_count(&self.fields) != 1 {
            self.identity = Arc::default();
        }
        let fields = Arc::make_mut(&mut self.fields);
        fields.logical_bytes.take();
        fields.initialized[index] = true;
        fields.values.get_mut(index)
    }

    pub(crate) fn cached_bytes(&self) -> Option<u64> {
        self.fields.logical_bytes.get().copied()
    }

    pub(crate) fn cache_bytes(&self, bytes: u64) {
        let _ = self.fields.logical_bytes.set(bytes);
    }

    pub(crate) fn field_index(&self, name: &str) -> Option<usize> {
        self.names
            .binary_search_by(|candidate| candidate.as_str().cmp(name))
            .ok()
    }

    pub(crate) fn field_at(&self, index: usize) -> Option<&Value> {
        self.fields.values.get(index)
    }

    pub fn get(&self, name: &str) -> Option<&Value> {
        self.field_at(self.field_index(name)?)
    }

    pub fn contains_key(&self, name: &str) -> bool {
        self.field_index(name).is_some()
    }

    pub fn len(&self) -> usize {
        self.fields.values.len()
    }

    pub fn is_empty(&self) -> bool {
        self.fields.values.is_empty()
    }

    pub fn values(&self) -> std::slice::Iter<'_, Value> {
        self.fields.values.iter()
    }

    pub fn keys(&self) -> std::slice::Iter<'_, String> {
        self.names.iter()
    }

    pub fn iter(
        &self,
    ) -> std::iter::Zip<std::slice::Iter<'_, String>, std::slice::Iter<'_, Value>> {
        self.names.iter().zip(self.fields.values.iter())
    }
}

impl From<BTreeMap<String, Value>> for StructStorage {
    fn from(fields: BTreeMap<String, Value>) -> Self {
        let (names, values): (Vec<_>, Vec<_>) = fields.into_iter().unzip();
        let initialized = vec![true; values.len()];
        Self::with_layout(Arc::new(names), values, initialized)
    }
}

impl Index<&str> for StructStorage {
    type Output = Value;
    fn index(&self, name: &str) -> &Value {
        self.get(name).expect("field belongs to struct layout")
    }
}

impl IntoIterator for StructStorage {
    type Item = (String, Value);
    type IntoIter = std::iter::Zip<std::vec::IntoIter<String>, std::vec::IntoIter<Value>>;

    fn into_iter(self) -> Self::IntoIter {
        Arc::unwrap_or_clone(self.names)
            .into_iter()
            .zip(Arc::unwrap_or_clone(self.fields).values)
    }
}

impl<'a> IntoIterator for &'a StructStorage {
    type Item = (&'a String, &'a Value);
    type IntoIter = std::iter::Zip<std::slice::Iter<'a, String>, std::slice::Iter<'a, Value>>;

    fn into_iter(self) -> Self::IntoIter {
        self.iter()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{types::TypeIdentity, value::Data};

    #[test]
    fn logical_size_cache_follows_nested_copy_on_write() {
        let nested = Value {
            typ: TypeIdentity::Any,
            data: Data::Struct(BTreeMap::from([("text".into(), Value::string("abc"))]).into()),
        };
        let original = Value {
            typ: TypeIdentity::Any,
            data: Data::Struct(BTreeMap::from([("nested".into(), nested)]).into()),
        };
        let before = original.logical_bytes().unwrap();
        let mut copy = original.clone();
        assert_eq!(copy.logical_bytes().unwrap(), before);
        let Data::Struct(fields) = &mut copy.data else {
            unreachable!()
        };
        let Data::Struct(nested) = &mut fields.get_mut("nested").unwrap().data else {
            unreachable!()
        };
        *nested.get_mut("text").unwrap() = Value::string("a longer string");
        assert_eq!(copy.logical_bytes().unwrap(), before + 12);
        assert_eq!(original.logical_bytes().unwrap(), before);
        assert_eq!(copy.logical_bytes().unwrap(), before + 12);
    }

    #[test]
    fn nested_arrays_and_fields_are_independent_after_value_copy() {
        let original = StructStorage::from(BTreeMap::from([
            ("Name".into(), Value::string(b"original".to_vec())),
            (
                "Items".into(),
                Value {
                    typ: TypeIdentity::Any,
                    data: Data::Array(vec![Value::int(42)].into()),
                },
            ),
        ]));
        let mut copy = original.clone();
        let Data::Array(items) = &mut copy.get_mut("Items").unwrap().data else {
            panic!("expected array")
        };
        Arc::make_mut(items)[0] = Value::int(99);
        *copy.get_mut("Name").unwrap() = Value::string(b"copy".to_vec());
        let Data::Array(items) = &original["Items"].data else {
            panic!("expected array")
        };
        assert_eq!(items[0].integer().unwrap(), 42);
        assert_eq!(original["Name"].to_string(), "original");
        assert_eq!(copy["Name"].to_string(), "copy");
    }
}
